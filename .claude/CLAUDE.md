# iam-token-service — context for Claude

Custodian of the platform-automation service account's rotating credential
material for the Tender Management SaaS Platform's IAM subsystem: issues,
rotates, and revokes the per-tenant `platform-automation` Keycloak client's
keys, custodies the private-key plaintext exclusively in OpenBao (never
Postgres), and drives the tenant-offboarding credential cleanup cascade.
Since **EXT-6** (rev 1.3), the mechanism is Keycloak's `client-jwt`/JWKS
authenticator, not a shared secret: TS-1 generates an RSA-2048 keypair and
this service serves the public half itself (`GET
.../service-accounts/platform-automation/jwks.json`, unauthenticated by
design — Keycloak's own outbound fetch carries no caller identity); the
Realm Provisioner never receives key material, only triggers Keycloak's
key-cache refresh (RP-17, `ClearServiceAccountKeysCache`) after every
rotate/revoke.

The signed-off design is `docs/lld/iam-lld-token-service.md` (rev 1.3,
Approved) — it is the tie-breaker on any discrepancy between this file and
reality. A frozen name (LLD §25) or a resolved open question (§16) cannot
be changed in place; that requires a new LLD revision. Everything else
(prose sections, the Decision Register §22, runbooks §24) is normal
living-document maintenance and has been kept current through an
implementation-phase hardening pass (LLD §22 TS-D13) and a
production-readiness review after the EXT-6/cadence-scheduler work landed
(LLD §22 TS-D15).

## What this repo is

**Owns:** credential lifecycle (issue/rotate/revoke) for the automation
principal; the service-account principal registry (metadata only); secret
material custody in OpenBao; credential-lifecycle events on
`iam-serviceaccount-events`; tenant-offboarding credential cleanup.

**Does NOT own:** minting/deleting the Keycloak client or **setting** its
secret at Keycloak (Realm Provisioner is the sole Keycloak Admin API
writer — this service has zero `gocloak` dependency, TS-INV-1); human-user
tokens/login; authorization decisions (the automation principal carries no
roles, O&M AUTH-9); membership grants; tenant-owned bots or user PAT
self-service (documented Phase-2 extension of the same schema, not built).

## Common commands

| Command | Purpose |
|---|---|
| `make setup` | Copy `.env-example` → `.env`, install git hooks |
| `make docker-up` | Start infra only (Postgres/PgBouncer/OpenBao/Floci) |
| `make run` / `make run-consumer` | Run `cmd/server`/`cmd/consumer` natively against that infra |
| `make build` | Compile all four binaries |
| `make test` / `make test-unit` | Full parallel suite / unit only, no containers |
| `make test-ci` | Coverage-instrumented, merged pipeline (what CI runs) |
| `make lint` | `go-arch-lint` + `golangci-lint` |
| `make gates` | `no-gocloak`, `no-secret-log`, `set-local-only`, `gincommon-obs`, `metrics-taxonomy` |
| `make sast` | `gosec` Go-code static analysis |
| `make vuln-check` | `govulncheck` |
| `make swag` | Regenerate `docs/swagger/` from handler annotations |
| `make ci` | `tidy fmt-check vet lint gates test-ci build` — the full local rehearsal of CI |
| `make godoc` | Serve package documentation locally |

## Architecture

Clean Architecture (`domain` → `port` → `service` → adapters → `cmd`),
structurally enforced by `.go-arch-lint.yml`. Four binaries
(`cmd/server`, `cmd/consumer`, `cmd/rotator`, `cmd/scheduler`) share one
image; `cmd/rotator` may **not** depend on `service` — it drives
`postgres`/`openbao` directly under a `BYPASSRLS` connection for
cross-tenant enumeration (RLS-7), a privilege no HTTP/event path may hold.
`cmd/scheduler` (§16 TSQ-6 Resolved, TS-D14) shares that same `BYPASSRLS`
enumeration for its own due-list scan, but — unlike `cmd/rotator` — *is*
allowed to depend on `service`: it drives automatic cadence-based
rotation through the ordinary `CredentialService.IssueOrRotate`, then
calls RP-17 itself; `cmd/rotator`'s sweep now does the same RP-17 call
after every automatic revoke (TS-D15) — neither path is complete at
Keycloak without it under EXT-6 (no self-expiring key-cache TTL exists).
Full diagrams and per-mechanism deep-dives: root `ARCHITECTURE.md`. Terse
package-layout/dependency-rule reference for quick lookups:
`.claude/architecture.md`.

## Key files to know

- **`internal/core/service/credential_service.go`** — TS-1 (`issueOrRotate`)
  and TS-2 (`revokeCredential`). Material-first write ordering (OpenBao
  write/delete precedes the Postgres commit). `replayIssueOrRotate`
  handles `rotation_id` idempotency; a replay against a since-`revoked`
  row returns `409 credential_replay_revoked`, not a misleading `502`
  (LLD TS-D13). `revokeCredential` re-checks after an
  `optimistic_lock_conflict` and returns the idempotent outcome if a
  concurrent revoke (another call, or the overlap sweep) already won the
  race.
- **`internal/adapter/outbound/postgres/{credential,principal,reconciler}_repository.go`,
  `db.go`** — `withPool` joins whatever `*pgx.Tx` is already in `ctx`
  (`port.TxFromContext`) instead of always opening a real pool connection
  — the seam `internal/adapter/outbound/postgres/repository_errors_test.go`
  uses to force every driver-error branch with a hand-rolled fake `pgx.Tx`,
  no real Postgres needed. `credential_repository.go`'s `Insert` uses a
  `SAVEPOINT`/`ROLLBACK TO SAVEPOINT` around the write specifically so the
  `active_rotation_id` enrichment lookup can still run after a real
  `23505` (Postgres aborts the whole transaction on any statement failure
  otherwise).
- **`internal/adapter/outbound/openbao/client.go`** — Kubernetes-auth-only
  (no static token field exists in `Config`). `kvClient()` clones the SDK
  client per call (`c.baoClient.Clone()`) before `SetToken`, not mutating
  the shared client directly — removes a token race under concurrent
  requests at zero connection-setup cost (LLD TS-D13).
- **`internal/adapter/inbound/consumer/{offboarding_consumer,dedup}.go`** —
  the one inbound subscription (`TenantMembershipsPurged`). A
  missing/invalid envelope id or unrecognized event type is **acked, not
  retried** (`ackUnknown`/`dedup.go`) — a producer schema addition must
  never DLQ-storm this queue. Material-first delete ordering here too
  (OpenBao deletes complete before the Postgres cascade commits).
- **`cmd/consumer/{inbound_schema,dlq}.go`** — the consumer's pipeline,
  outermost first: DLQ router → cascade metrics → `validateConsumed` →
  `Handle`, on an SQS consumer built by `buildSQSConsumer` with
  `events.WithConsumerCodec(eventbus.GlueDecoder{})` (O&M publishes
  `TenantMembershipsPurged` Glue-encoded — without the decoder every one
  failed decode into the DLQ). `validateConsumed` checks the payload against
  the embedded `tenant_memberships_purged.json` (`ValidatingCodec.Validate`,
  `ErrNoSchema` → pass-through to `ackUnknown`); a violation increments
  `iam_token_service_consumed_schema_violations_total` and
  `routeRejectsToDLQ` sends it straight to the DLQ (`DLQReason=
  schema_violation`, URL from the queue's `RedrivePolicy`, `sqs:SendMessage`
  grant `OffboardingDLQPermanentRejects`) and acks; falls back to normal
  redrive if the DLQ can't be resolved or the send fails.
- **`internal/adapter/outbound/eventbus/glue_codec.go`** — `GlueCodec`
  resolves each produced schema's version UUID **once at startup by
  definition** (`glue:GetSchemaByDefinition`, exact `schema-gov register`
  compact form via `registeredDefinition`, Python-parity-tested), must be
  `AVAILABLE`; no refresher, no per-event Glue call, unregistered definition
  fails startup. `GlueDecoder` — decode-only consumer codec.
- **`internal/adapter/outbound/metrics/metrics.go`** — the Enterprise
  Platform Observability Standard's 3-tier taxonomy. `Register(environment)`
  centrally injects `domain`/`service`/`environment` labels — instrumentation
  call sites never set them. Three metrics are dual-emitted (legacy
  Tier-3 kept running) alongside a registry-proposed Tier-1/Tier-2
  equivalent — see `docs/observability-registry-proposals.md`.
- **`cmd/rotator/{sweep,orphan_reconciler,prune}.go`** — run-to-completion
  CronJob body: overlap-expiry sweep, orphan-material reconciler, retention
  prune, one binary invocation per fire. `sweep.go`'s
  `revokeExpiredRotating` reclassifies a write-time
  `optimistic_lock_conflict` as `Skipped`, not `Failed` — the same
  benign-race handling as TS-2's revoke, just on the other side. Since
  TS-D15, it also calls RP-17 (`RealmProvisionerClient.RefreshKeys`)
  after every commit; a resulting RP-17 failure is `Failed`, not
  swallowed, since the row is already `revoked` and never re-enumerated.
- **`cmd/scheduler/{main,scan,helpers}.go`** — run-to-completion CronJob:
  scans `idx_sac_next_rotation` for `active` rows past due, calls
  `CredentialService.IssueOrRotate` directly (unlike `cmd/rotator`), then
  RP-17. An `IssueOrRotate`-committed-but-RP-17-failed outcome is a
  documented, page-worthy two-halves gap (`cadence_rotation_total{result="failed"}`)
  — `next_rotation_at` has already advanced, so it will NOT self-heal.
- **`internal/core/service/jwks_service.go`,
  `internal/adapter/inbound/http/jwks_handler.go`** — EXT-6's JWKS route.
  `PublicKeys` returns a `skipped` count (not just a log line) for any
  live credential whose OpenBao material was unreadable — the handler
  turns that into `iam_token_service_jwks_key_errors_total` (page-worthy:
  a live credential going unserved is a real Keycloak auth outage, not a
  routine "unknown tenant" empty response). The handler also carries a
  process-wide rate limiter (`WithRateLimit`) and `Cache-Control`/
  `X-Content-Type-Options` headers — this is the one unauthenticated,
  fully public route on the service.
- **`internal/adapter/outbound/realmprovisioner/client.go`** — the RP-17
  client both `cmd/rotator` and `cmd/scheduler` call. Retries once (2
  attempts total) on a plausibly-transient failure with a short backoff —
  deliberately small, because it multiplies directly against how many
  due/revoked rows one CronJob run can process inside
  `activeDeadlineSeconds` (see `ROTATOR_RUN_TIMEOUT`/`SCHEDULER_RUN_TIMEOUT`'s
  own default, tuned in the same review to stay under that same k8s
  deadline so the graceful in-process exit path always wins).
- **`internal/core/domain/errors.go`** — the frozen §17 taxonomy plus two
  additive codes (`db_unavailable`, `credential_replay_revoked`), each
  explicitly commented as additive-not-frozen.
- **`internal/adapter/outbound/postgres/db.go`'s `wrapConnErr`,
  `internal/adapter/inbound/http/errors.go`'s `HandleError`** (TS-D16) —
  `db_unavailable` (503) is classified by **positive SQLSTATE
  identification only** (class `08`/`53`/`57`/`58`, or a closed pool) —
  never a broad "looks like a network error" heuristic, which used to
  risk discarding a caller's real business error under a misleading 503.
  `HandleError` independently re-classifies a leaked `*pgconn.PgError` of
  the same classes as defense-in-depth against a case `wrapConnErr` itself
  misses.

## Data model

Two tenant-scoped tables under `FORCE ROW LEVEL SECURITY`
(`service_account_principals`, `service_account_credentials`), plus
`outbox_events` (owned by `platform-events`) and `processed_events`
(RLS-exempt). Two migrations so far (dev stage, nothing deployed): the
base schema, plus `000002_rotation_cadence` (§16 TSQ-6 Resolved —
`rotation_cadence_days`/`next_rotation_at` + `idx_sac_next_rotation`).
Full detail in **[.claude/database-schema.md](database-schema.md)**.

## API & events

7 routes (TS-1..TS-6 plus the EXT-6 JWKS route) under
`/api/v1/internal/*`. TS-6 (`GET …/service-accounts/platform-automation`,
TS-D17) is the reverse of TS-5: it gives the Workflow Service's connector
workers a tenant's automation `principal_sub` from the tenant id alone.
The sub is stable across rotation but changes on an RP-3/RP-4 re-mint;
`principal_id` doesn't. `ServiceAccountRegistered`'s `keycloak_client_id`
now accepts the tenant-scoped shape too. It was a `const`, which 500'd
every RP-1/RP-4 registration. TS-5 (`GET …/service-accounts?principal_sub=<uuid>`,
TS-D16) finds a principal by Keycloak sub instead of this service's own
internal id — org-membership's AUTH-9 non-member defense-in-depth check
needs it, since a subject's sub is the only identifier that check ever
sees. The JWKS route is this service's one intentionally public,
unauthenticated-by-header surface (§5.6) — every other route requires the
reserved system principal. One inbound event subscription
(`TenantMembershipsPurged`), 5 frozen outbound events on
`iam-serviceaccount-events` (single producer). Full detail in
**[.claude/api-events.md](api-events.md)**.

## Request flows & concurrency

TS-1 issue/rotate, TS-2 revoke, the offboarding cascade, and the
reconciler sweep, each with their failure-mode/idempotency handling
(including the LLD TS-D13 race fixes). Full detail in
**[.claude/request-flows.md](request-flows.md)**.

## Operations

Security (RLS/GUC/OpenBao-auth), the 3-tier metrics taxonomy, config env
vars, deployment topology, testing strategy. Full detail in
**[.claude/operations.md](operations.md)**.

## See also

- [architecture.md](architecture.md) — package layout, dependency rules
- [operations.md](operations.md) — security, observability, config, deployment, testing
- [request-flows.md](request-flows.md) — per-flow walkthroughs, concurrency, failure handling
- [database-schema.md](database-schema.md) — tables, RLS, triggers, invariants
- [api-events.md](api-events.md) — endpoint catalogue, event contract
- [`docs/lld/iam-lld-token-service.md`](../docs/lld/iam-lld-token-service.md) — the signed-off LLD (source of truth)
- [`docs/observability-registry-proposals.md`](../docs/observability-registry-proposals.md) — Tier-1/Tier-2 metric registry submissions
- [`../README.md`](../README.md) / [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — human-facing onboarding and deep-dive docs
