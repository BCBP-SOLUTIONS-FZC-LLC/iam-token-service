# iam-token-service — context for Claude

Custodian of the platform-automation service account's rotating credential
material for the Tender Management SaaS Platform's IAM subsystem: issues,
rotates and revokes the per-tenant `platform-automation` Keycloak client's
keys, custodies the private-key plaintext exclusively in OpenBao (never
Postgres), and drives the tenant-offboarding credential cleanup cascade.
Since **EXT-6** (rev 1.3) the mechanism is Keycloak's `client-jwt`/JWKS
authenticator, not a shared secret: TS-1 generates an RSA-2048 keypair and
this service serves the public half itself (`GET
.../service-accounts/platform-automation/jwks.json`, unauthenticated by
design — Keycloak's outbound fetch carries no caller identity). The Realm
Provisioner never sets key material at Keycloak; it only clears Keycloak's
key cache (RP-17, `ClearServiceAccountKeysCache`), which `cmd/rotator` and
`cmd/scheduler` call themselves after their own revokes/rotations.

The signed-off design is `docs/lld/iam-lld-token-service.md` (rev 1.4,
Approved) — the tie-breaker on any discrepancy. A frozen name (LLD §25) or
a resolved open question (§16) cannot change in place; that needs a new LLD
revision. Prose, the Decision Register (§22) and the runbooks (§24) are
living documents. The §22 entries TS-D13..TS-D23 are the changelog of the
hardening passes since sign-off; TS-D21/22/23 are the most recent
production-readiness passes.

## What this repo is

**Owns:** credential lifecycle (issue/rotate/revoke) for the automation
principal; the service-account principal registry (metadata only); secret
material custody in OpenBao; the public JWKS for those keys;
credential-lifecycle events on `iam-serviceaccount-events`;
tenant-offboarding credential cleanup.

**Does NOT own:** minting/deleting the Keycloak client or writing anything
at Keycloak (Realm Provisioner is the sole Keycloak Admin API writer —
zero `gocloak` dependency, TS-INV-1); human-user tokens/login;
authorization decisions (the automation principal carries no roles, O&M
AUTH-9); membership grants; tenant-owned bots or user PAT self-service
(Phase-2 extension of the same schema, not built).

## Common commands

| Command | Purpose |
|---|---|
| `make setup` | Copy `.env-example` → `.env`, install git hooks |
| `make docker-up` / `make docker-down` | Start/stop local infra (Postgres, PgBouncer, OpenBao, Floci SNS/SQS/Glue + UI) |
| `make run` / `make run-consumer` / `make run-scheduler` | Run `cmd/server` / `cmd/consumer` / `cmd/scheduler` natively against that infra |
| `make docker-run-rotator` / `make docker-run-scheduler` | One CronJob-style run in Docker Compose |
| `make build` | Compile all four binaries into `bin/` |
| `make test` | Unit + contract + postgres + integration in parallel (testcontainers) |
| `make test-unit` / `test-contract` / `test-postgres` / `test-integration` / `test-e2e` | One suite |
| `make test-ci` | Coverage-instrumented, merged pipeline (what CI runs; CI gates ≥ 98%) |
| `make test-smoke IMAGE_TAG=… BINARY=…` | Smoke-test a built image |
| `make lint` / `make arch-lint` | `golangci-lint` (plain + all test tags) / `go-arch-lint` boundary check |
| `make gates` | Ten invariant gates: `no-gocloak` (TS-INV-1), `no-secret-log` (TS-INV-2), `set-local-only` (RLS-6), `gincommon-obs` (negative: rejects slog/stdlib-log/direct-zap/DefaultRegisterer/hand-rolled TracerProvider), `gincommon-wired` (positive: all 8 required gincommon init functions present in every binary), `pgcommon-obs` (negative: rejects raw pgxpool/pgx.Connect/database/sql), `pgcommon-wired` (positive: ConfigFromEnv/NewPool/GUCSetFromContext/NewLoggerAdapter in every binary), `platform-events-obs` (negative: rejects sqs.ReceiveMessage/sns.Publish), `platform-events-wired` (positive: server wires SNS+outbox; consumer wires SQS+inbox), `metrics-taxonomy` (Tier-1/2/3 prefixes, `_total`/`_seconds` suffixes) |
| `make metrics-lint` / `make metrics-inventory` | `metricslint` on a real scrape + `metric-registry.md` drift check / regenerate it |
| `make sast` / `make vuln-check` | `gosec` / `govulncheck ./...` |
| `make swag` / `make swag-check` | Regenerate / staleness-check `docs/swagger/` |
| `make schema-validate` / `schema-register` / `schema-verify` / `schema-prune` | Glue schema governance (schema-gov) |
| `make ci` | `tidy fmt-check vet lint arch-lint gates metrics-lint test-ci build` — local CI rehearsal |
| `make godoc` | Serve package documentation locally |

## Architecture

Clean Architecture (`domain` → `port` → `service` → adapters → `cmd`),
enforced by `.go-arch-lint.yml`. Four binaries share one distroless image:

- `cmd/server` — the HTTP API (7 routes) and the outbox relay, plus four
  background loops: rotation-overlap exporter, RLS-violation exporter,
  `keys_refresh_pending` exporter, JWKS known-tenant refresher. With
  `MIGRATE_ONLY=true` it applies migrations and exits (the Helm hook Job).
- `cmd/consumer` — the SQS offboarding consumer.
- `cmd/rotator` (CronJob) — RP-17 marker retry, overlap-expiry sweep,
  orphan-material reconciler, retention prune.
- `cmd/scheduler` (CronJob) — RP-17 marker retry, cadence rotation.

`cmd/rotator` may **not** import `service`: it enumerates across tenants
on the `BYPASSRLS` reconciler pool and writes through RLS-scoped app-pool
transactions (RLS-7). `cmd/scheduler` uses the same enumeration but *may*
import `service` and rotates through `CredentialService.IssueOrRotate`
(§16 TSQ-6 Resolved, TS-D14). Diagrams: root `ARCHITECTURE.md`; layout and
dependency rules: [architecture.md](architecture.md).

## Key files to know

- **`internal/core/service/credential_service.go`** — TS-1
  (`issueOrRotate`) and TS-2 (`revokeCredential`). Each runs in one
  transaction that first locks the principal row (`LockForUpdate`,
  `SELECT … FOR UPDATE`; `55P03` → `409 rotation_in_flight` with best-effort
  `details.active_rotation_id`). Version = `MaxVersion + 1` over every row;
  the keypair is generated before the transaction; the in-lock OpenBao
  write/delete is bounded at 5s; older open overlaps are closed (TS-INV-3);
  material-first ordering (OpenBao before commit). `ExpectActiveVersion`
  (scheduler only) turns a since-rotated principal into a no-op. Replays:
  within `ROTATION_REPLAY_WINDOW` same key; past it `409
  credential_replay_expired`; revoked `409 credential_replay_revoked`; each
  logged and counted (TS-D13/21/22/23).
- **`internal/adapter/outbound/postgres/db.go`** — `TxRunner` (retries
  `40001`/`40P01`), `withPool` (joins the tx in ctx — the seam
  `repository_errors_test.go` uses with a fake `pgx.Tx`),
  `wrapConnErrCtx`: `55P03` → `rotation_in_flight`; SQLSTATE class
  `08`/`53`/`57`/`58` or a closed pool → `503 db_unavailable` (positive
  identification only, TS-D16); a `57014` caused by the caller's own ctx
  passes through. `http/errors.go`'s `HandleError` re-classifies a leaked
  `PgError` the same way. `credential_repository.go`'s `Insert` wraps the
  INSERT in a `SAVEPOINT` so the `active_rotation_id` lookup still runs
  after a `23505`.
- **`internal/adapter/outbound/postgres/keys_refresh_repository.go`,
  `cmd/{rotator,scheduler}/keys_refresh.go`** — the durable "RP-17 owed"
  marker (`keys_refresh_pending`, TS-D22/23). `MarkPending` = committed
  marker (`intent_until` NULL); `MarkIntent` = scheduler's pre-rotation
  intent, never downgrading a committed marker; `Clear` deletes only up to
  the `requested_at` observed; `ListPending` (reconciler pool) skips live
  intents. Each run starts with `retryPendingKeyRefreshes` (≤ half the run
  budget); a tenant with no principal left whose RP-17 fails is dropped.
- **`internal/adapter/outbound/postgres/jwks_tenants_repository.go`,
  `internal/adapter/inbound/http/jwks_handler.go`,
  `internal/core/service/jwks_service.go`** — EXT-6's JWKS route. Three
  token buckets: per-tenant (LRU, 5/10) for every tenant, then global
  (20/40) for *known* tenants or unknown (2/5) for the rest; 429
  `rate_limited` is counted in `jwks_rate_limited_total` and
  `jwks_rate_limited_by_bucket_total{bucket}`. "Known" = the DB list of
  tenants with an `active`/`rotating` credential (reloaded every
  `JWKS_KNOWN_TENANTS_REFRESH` over the BYPASSRLS reconciler pool via
  `withReadOnlyPool`) plus tenants served since the last reload. Expired
  overlap keys are skipped; an unreadable key counts in
  `jwks_key_errors_total`; the active key (or every key) unreadable →
  `503 jwks_keys_unavailable`. `Cache-Control: no-cache`, `nosniff`.
  `JWKSHandler.Health()` implements the `/readyz` pinger (TS-RP-GAP-001):
  the pod stays not-ready until the first successful DB refresh so newly
  provisioned tenants are served from the global bucket immediately.
  Tracked by `jwks_known_tenants_refresh_total{result}` and
  `jwks_known_tenants_last_refresh_age_seconds`.
- **`internal/adapter/inbound/http/{router,middleware}.go`** — 1 MiB body
  cap → gincommon `ObservabilityMiddlewares` (+ 30s `RequestTimeout`) →
  for `/api/v1/internal`: `RequireIdentityHeaders` (exactly one UUID
  `x-user-id`/`x-tenant-id`, else `401 missing_identity_headers`; drops
  `x-tenant-roles`) → gincommon `RequireAuth`/`ContextMiddleware` →
  `GUCBridgeMiddleware` (x-user-id must be the iam-system UUID) →
  `RequireJSONContentType` (415) → `RequireTenantPathMatch` (403
  `tenant_path_mismatch`). The JWKS route sits outside that group.
- **`internal/adapter/outbound/openbao/client.go`** — Kubernetes auth only
  (`OPENBAO_K8S_TOKEN_PATH`, Helm: projected token, audience `openbao`);
  single-flight login detached from the caller's ctx; token margin
  `min(30s, lease/2)`; one re-login + retry on 403; per-call SDK client
  clone (`kvFor`); `BAO_CACERT` honoured by the SDK; Delete removes KV v2
  metadata (no soft delete).
- **`internal/adapter/inbound/consumer/{offboarding_consumer,dedup}.go`,
  `postgres/inbox_repository.go`** — the `TenantMembershipsPurged`
  cascade. platform-events' `pkg/inbox` claims `processed_events` first and
  runs the whole cascade in one RLS-scoped transaction (TS-D19):
  `LockByTenant` (FOR UPDATE), collect paths, delete OpenBao material
  (every row's path, then the tenant's whole OpenBao subtree), delete rows,
  enqueue `ServiceAccountRevoked`, commit. A `TenantMembershipsPurged` with
  a missing/non-UUID envelope id is an error the DLQ router dead-letters;
  an unknown event type is acked (`ackUnknown`, `event_type` label bounded
  to a closed set, else `other`).
- **`cmd/consumer/{main,inbound_schema,dlq}.go`** — pipeline outermost
  first: DLQ router → cascade metric → `validateConsumed` → `Handle`, on an
  SQS consumer with `GlueDecoder` (O&M publishes Glue-encoded). The DLQ
  router sends `schema_violation` and `invalid_envelope_id` rejects
  straight to the DLQ (URL from `RedrivePolicy`, IAM Sid
  `OffboardingDLQPermanentRejects`), counts
  `consumer_dlq_rejects_total{reason}` and acks; it falls back to SQS
  redrive if the DLQ can't be resolved or the send fails. SQS defaults:
  concurrency 2, visibility 60s, handler 45s, drain 15s, queue-depth
  sampling 60s. SIGTERM: `/readyz` 503 for `SHUTDOWN_DRAIN_DELAY`, then
  stop; exits 1 if the consume loop dies on its own.
- **`cmd/server/exporters.go`** — three loops: `rotation_overlap_active`
  and the `rls_violation_log` → `iam_rls_violations_total` counter (id
  cursor; every replica counts the same rows — use `max`, not `sum`) on the
  reconciler pool, and the `keys_refresh_pending` / oldest-age gauges.
- **`cmd/rotator/{sweep,orphan_reconciler,prune}.go`** and
  **`cmd/scheduler/scan.go`** — tenant-by-tenant batches (`*_BATCH_LIMIT`,
  500); a tenant starts only with 30s of budget left; RP-17 inline after
  each tenant on a detached 10s context; SIGTERM finishes the current
  tenant. Sweep writes the marker in the revoke tx and does not take the
  principal lock (an `optimistic_lock_conflict` is `Skipped`). Scheduler
  skips (`principal_revoked`/`not_found`, `rotation_in_flight`,
  `optimistic_lock_conflict`). A failed RP-17 is `Failed` (pages) but
  self-heals via the marker. Orphan reconciler: per-principal budget with a
  rotating start offset; orphan deletes and `missing_material` checks run
  under the principal lock (busy → `Skipped`). Prunes are budget-checked
  (out of time = Deferred). Any Failed (or `missing_material`) → exit 1.
- **`internal/adapter/outbound/realmprovisioner/client.go`** — RP-17:
  `POST …/tenants/:id/service-account/keys/refresh`, 3s per attempt, 2
  attempts, retries transport errors and 429/502/503/504, any 2xx is
  success; instrumented as `platform_dependency_request_seconds{dependency="realm_provisioner",operation="refresh_keys"}`.
- **`internal/adapter/outbound/metrics/metrics.go`** — 3-tier taxonomy;
  every collector carries gincommon's `{domain, service, environment}`
  const labels. `Register` adopts platform-events' shared Tier 1
  collectors (`registerShared`). Only OpenBao latency is dual-emitted (Tier
  3 `openbao_call_duration_seconds` + Tier 1
  `platform_dependency_request_seconds`). `iam_offboarding_cascade_total`
  stays proposed and unemitted.
- **Observability wiring (all four `cmd/*/main.go`)** —
  `InitTracingWithConfig` (name from `APP_NAME`) → `ObservabilityMiddlewares`
  → `metrics.InitLibraryMetrics` → `metrics.Register` → pools. `/metrics`
  on `METRICS_PORT` via `gincommon.MetricsHandler()`; the CronJobs serve it
  for `*_METRICS_SCRAPE_GRACE` after the run.
- **`internal/adapter/outbound/eventbus/glue_codec.go`** — resolves each
  produced schema's version once at startup by definition (must be
  `AVAILABLE`; an unregistered definition fails startup). `GlueDecoder` is
  the consumer's decode-only codec.
- **`internal/core/domain/errors.go`** — the frozen §17 codes plus additive
  ones (`db_unavailable`, `credential_replay_revoked`,
  `credential_replay_expired`, `jwks_keys_unavailable`); the HTTP layer
  also writes `rate_limited`, `tenant_path_mismatch`,
  `unsupported_media_type`, `internal_error`.
- **`deploy/helm/templates/validate.yaml`** — render guards outside
  local/dev/test: `authorizationPolicy.enabled` (with `meshNamespaces` and
  `keycloakNamespaces`), `events.topicArn`, `sqs.queueUrl`, docs need auth;
  `perWorkload=false` with `workloadAnnotations` is rejected (`migrate`
  allowed). NetworkPolicy selectors/CIDRs are guarded in
  `networkpolicy.yaml`.

## Data model

Two tenant-scoped tables under `FORCE ROW LEVEL SECURITY`
(`service_account_principals`, `service_account_credentials`), plus
RLS-exempt `processed_events`, `rls_violation_log`, `keys_refresh_pending`
and platform-events' `outbox_events`/`outbox_dead_letters`. Five
migrations (dev stage, nothing deployed): `000001_schema`,
`000002_rotation_cadence` (TSQ-6), `000003_rls_violation_log`,
`000004_hardening` (outbox dead-letter/sequence grants, `app_tenant_id()`
search_path pinned + PUBLIC revoked, RLS-log prune TTL ≥ 7 days, TS-D21),
`000005_keys_refresh_pending` (TS-D22/23). Full detail:
[database-schema.md](database-schema.md).

## API & events

7 routes under `/api/v1/internal/tenants/:id/…`: TS-4 register, TS-1
issue/rotate, TS-2 revoke, TS-3 read, TS-5 find by `principal_sub`
(TS-D16), TS-6 read the platform-automation principal (TS-D17), and the
EXT-6 JWKS route (public). Every TS route needs `x-user-id` = the iam-system
UUID `00000000-0000-0000-0000-0000000000a1` and `x-tenant-id` = the path
tenant. One inbound subscription (`TenantMembershipsPurged`), 5 frozen
outbound events on `iam-serviceaccount-events` via the transactional
outbox. Full detail: [api-events.md](api-events.md).

## Request flows & concurrency

TS-4, TS-1, TS-2, offboarding, sweep, orphan reconciler, prune, scheduler
and JWKS, with their locking, idempotency and failure handling:
[request-flows.md](request-flows.md).

## Operations

Security, metrics/alerts/SLOs, every env var, Helm values, deployment and
testing: [operations.md](operations.md). Alerts live in
`deploy/monitoring/app-alerts.yml` (mirrored by hand in Helm
`prometheusrule.yaml`), SLOs in `slo-rules.yml` / `prometheusrule-slo.yaml`,
runbooks in `docs/observability/runbooks.md`.

## See also

- [architecture.md](architecture.md) — package layout, dependency rules
- [operations.md](operations.md) — security, observability, config, deployment, testing
- [request-flows.md](request-flows.md) — per-flow walkthroughs, concurrency, failure handling
- [database-schema.md](database-schema.md) — tables, RLS, grants, triggers, functions
- [api-events.md](api-events.md) — endpoints, headers, errors, events, consumer
- [`docs/lld/iam-lld-token-service.md`](../docs/lld/iam-lld-token-service.md) — the signed-off LLD (source of truth)
- [`docs/observability/`](../docs/observability/README.md) — Observability Standard implementation, generated `metric-registry.md`, `runbooks.md`, `migration.md`
- [`docs/observability-registry-proposals.md`](../docs/observability-registry-proposals.md) — Tier-1/Tier-2 metric registry submissions
- [`../README.md`](../README.md) / [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — human-facing onboarding and deep-dive docs
