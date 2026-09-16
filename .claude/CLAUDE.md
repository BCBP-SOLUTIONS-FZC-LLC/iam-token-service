# iam-token-service — context for Claude

Custodian of the platform-automation service account's rotating credential
material for the Tender Management SaaS Platform's IAM subsystem: issues,
rotates, and revokes the per-tenant `platform-automation` Keycloak client's
secret, custodies the plaintext exclusively in OpenBao (never Postgres),
and drives the tenant-offboarding credential cleanup cascade.

The signed-off design is `docs/iam-lld-token-service.md` (rev 1.0,
Approved) — it is the tie-breaker on any discrepancy between this file and
reality. A frozen name (LLD §25) or a resolved open question (§16) cannot
be changed in place; that requires a new LLD revision. Everything else
(prose sections, the Decision Register §22, runbooks §24) is normal
living-document maintenance and has been kept current through an
implementation-phase hardening pass (LLD §22 TS-D13).

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
| `make build` | Compile all three binaries |
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
structurally enforced by `.go-arch-lint.yml`. Three binaries
(`cmd/server`, `cmd/consumer`, `cmd/rotator`) share one image;
`cmd/rotator` may **not** depend on `service` — it drives
`postgres`/`openbao` directly under a `BYPASSRLS` connection for
cross-tenant enumeration (RLS-7), a privilege no HTTP/event path may hold.
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
  benign-race handling as TS-2's revoke, just on the other side.
- **`internal/core/domain/errors.go`** — the frozen §17 taxonomy plus two
  additive codes (`db_unavailable`, `credential_replay_revoked`), each
  explicitly commented as additive-not-frozen.

## Data model

Two tenant-scoped tables under `FORCE ROW LEVEL SECURITY`
(`service_account_principals`, `service_account_credentials`), plus
`outbox_events` (owned by `platform-events`) and `processed_events`
(RLS-exempt). One migration so far (dev stage, nothing deployed).
Full detail in **[.claude/database-schema.md](database-schema.md)**.

## API & events

4 routes (TS-1..TS-4) under `/api/v1/internal/*`, no public surface. One
inbound event subscription (`TenantMembershipsPurged`), 5 frozen outbound
events on `iam-serviceaccount-events` (single producer). No caching layer
in this service. Full detail in
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
- [`docs/iam-lld-token-service.md`](../docs/iam-lld-token-service.md) — the signed-off LLD (source of truth)
- [`docs/observability-registry-proposals.md`](../docs/observability-registry-proposals.md) — Tier-1/Tier-2 metric registry submissions
- [`../README.md`](../README.md) / [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — human-facing onboarding and deep-dive docs
