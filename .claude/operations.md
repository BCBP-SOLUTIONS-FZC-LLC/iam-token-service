# Operations

Section numbers mirror the LLD's own numbering (`docs/iam-lld-token-service.md`)
so this file stays traceable to the signed-off design. This is the
canonical source for exact metric/env-var names — README summarizes and
points here; the LLD is still the tie-breaker on any conflict.

## 10. Security

### Tenant isolation (three layers)

1. The reserved `iam-system` principal + target `x-tenant-id` on every
   internal route (RLS-5) — a request missing either is `401` before any
   DB checkout.
2. `FORCE ROW LEVEL SECURITY` + default-deny on both tenant-scoped tables,
   keyed on the transaction-local `app.tenant_id` GUC (`SET LOCAL`, never
   plain `SET` — RLS-6).
3. The composite FK `(principal_id, tenant_id)` — a cross-tenant credential
   row is structurally impossible.

### Network isolation

Mesh-mTLS only. `networkPolicy.ingressNamespaceSelector` (Helm) is a
**required** value — the render fails closed if left unset rather than
falling back to an unrestricted `{}` selector, which would match every
namespace in the cluster.

### Input validation

`rotation_id` must be a UUID; `overlap_seconds` clamped to `[0, 900]`
server-side regardless of what the caller sends; `principal_sub` must be a
UUID; `keycloak_client_id` validated against the frozen `platform-automation`
value. Path params typed-parsed; malformed → `400` before any DB checkout.

### Authorization matrix

| Route | Callers |
|---|---|
| TS-4 register | Realm Provisioner |
| TS-1 issue/rotate | Realm Provisioner, operators, `cmd/rotator`'s opportunistic sweep |
| TS-2 revoke | operators, the offboarding cascade, the overlap-expiry sweep |
| TS-3 read | Org & Membership, operators |

All routes require the reserved `iam-system` principal — no route accepts
a tenant-facing principal, and the automation principal itself holds no
roles (TS-INV-4).

### Secret handling

OpenBao KV v2 is the **only** home for plaintext (`iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>`,
frozen §25); Kubernetes-auth-only login (no static token field exists in
`openbao.Config`); TS-1 returns the plaintext exactly once, never
re-readable (TS-3 is metadata-only); this service never writes Keycloak
(TS-INV-1, CI-enforced via `no-gocloak`).

## 11. Observability

### SLOs

Not latency-critical (Keycloak is the hot token-issuance path, not this
service). Internal API availability 99.9% monthly; TS-1 p99 ≤ 500ms
excluding the OpenBao round-trip; TS-3 p99 ≤ 100ms.

### Metrics — Enterprise Platform Observability Standard

Three-tier taxonomy. `metrics.Register(environment)` centrally injects
`domain`/`service`/`environment` — instrumentation call sites never set
them. `make gates` → `metrics-taxonomy` enforces naming compliance in CI.

**Tier 3 (`iam_token_service_*`, frozen §25):**

| Metric | Type | Labels |
|---|---|---|
| `credentials_issued_total` | counter | `op` (issue/rotate/revoke) |
| `rotation_overlap_active` | gauge | — |
| `openbao_call_duration_seconds` | histogram | `op` (write/delete) — legacy, see Tier 1 below |
| `offboarding_cascade_total` | counter | `result` (ok/error) — legacy, see Tier 2 below |
| `rotation_sweep_total` | counter | `result` |
| `material_reconcile_total` | counter | `result` (orphan_deleted/missing_material/ok/error) |
| `processed_events_duplicates_total` | counter | `consumer` — legacy, see Tier 1 below |
| `unknown_event_acknowledged_total` | counter | `consumer`, `event_type` |

**Tier 1/Tier 2 (registry-proposed, dual-emitted alongside the legacy
metric above — not yet ratified, see `docs/observability-registry-proposals.md`):**

| Metric | Tier | Labels | Parallels |
|---|---|---|---|
| `platform_dependency_request_seconds` | 1 | `domain,service,environment,dependency,operation` | `openbao_call_duration_seconds` |
| `platform_duplicate_messages_total` | 1 | `domain,service,environment,queue` | `processed_events_duplicates_total` |
| `iam_offboarding_cascade_total` | 2 | `service,environment,outcome` | `offboarding_cascade_total` |

Shared-library metrics: `http_request_duration_seconds`/`http_requests_total`
(`platform-gincommon`), `outbox_*`/`sqs_*`/`events_*` (`platform-events`),
`pgcommon_pool_*` (`platform-pgcommon`, wired for both the app pool and the
`-reconciler`-suffixed BYPASSRLS pool). These predate the Standard and
carry a domain+service-combined `service` label — a known gap fixable only
in the shared library itself.

### Tracing

OTel spans `credential.issue_rotate`, `credential.revoke` (attrs
`tenant_id`/`principal_id`/`version`/`op`), `trace_id` propagated onto the
produced event envelope.

### Structured logs

`slog` JSON via `platform-gincommon`. Every operation logs
`tenant_id`/`principal_id`/`version`/`op`/`result`. **No credential field
is ever a log attribute** — CI gate `check-no-secret-log.py` (paren-matched,
case-insensitive, catches a multi-line log-call argument or a capitalized
Go identifier like `Secret`, not just a lowercase JSON-style key).

### Alerts

OpenBao failure-rate, stuck `rotating` versions, `material_reconcile_total{result="missing_material"}` > 0
(page — irrecoverable per-version data loss), offboarding-cascade DLQ
depth, `outbox_pending_total` growth (both a warning-stage backlog alert
and the later DLQ-depth alert), `pgcommon_pool_empty_acquire_total` growth
(pool exhaustion, either pool), offboarding-queue message age (dormant
until a CloudWatch exporter is deployed — SQS depth/age isn't available
in-process). Defined in `deploy/monitoring/app-alerts.yml`, mirrored in
`deploy/helm/templates/prometheusrule.yaml` (kept in sync by hand).

## 12. Configuration

Key env vars — see `.env-example` for the complete, currently-accurate
list:

| Var | Purpose |
|---|---|
| `APP_ENV`, `APP_PORT`, `METRICS_PORT` | Environment / API port / dedicated metrics port |
| `PG_HOST`/`PORT`/`USER`/`PASSWORD`/`DBNAME`/`SSLMODE`, `PG_MAX_CONNS`, `PG_BOUNCER_MODE` | App pool (`platform-pgcommon`) |
| `MIGRATION_DATABASE_URL` | Direct-port DSN, bypasses PgBouncer (session-scoped `pg_advisory_lock`) |
| `RECONCILER_DATABASE_URL` | `serviceaccount_reconciler` (BYPASSRLS, SELECT-only) pool for `cmd/rotator` |
| `OPENBAO_ADDR`, `OPENBAO_ROLE`, `OPENBAO_KV_MOUNT` | Kubernetes-auth login + KV v2 mount |
| `AWS_REGION`, `AWS_ENDPOINT_URL`, `GLUE_REGISTRY_NAME`, `SNS_TOPIC_SERVICEACCOUNT_ARN` (or `SNS_TOPIC_ARN` alias) | Produced events |
| `SQS_OFFBOARDING_QUEUE_URL`, `SQS_OFFBOARDING_CONCURRENCY` | The one inbound subscription |
| `ROTATION_DEFAULT_OVERLAP_SECONDS`, `ROTATION_DEFAULT_CADENCE_DAYS` | Rotation tuning (`overlap_seconds` still server-clamped to `[0,900]` regardless) |
| `PROCESSED_EVENTS_TTL_DAYS` | Dedup-ledger retention (must exceed SQS message lifetime) |
| `DOCS_ENABLED`, `DOCS_AUTH_TOKEN` | Swagger/AsyncAPI viewer gating |

## 13. Deployment and scaling

**Topology:** `Deployment` × 2 (server, consumer — HA not load), `CronJob` × 1
(rotator, singleton). `server`/`consumer` connect as `serviceaccount_app`
(RLS-scoped); `rotator` connects as `serviceaccount_reconciler` (BYPASSRLS,
read-only) for enumeration, then opens ordinary `serviceaccount_app`
transactions for the writes it makes.

**CronJob tuning:** `activeDeadlineSeconds: 240s`, deliberately shorter
than the 5-minute schedule so a run never eats into the next tick under
`concurrencyPolicy: Forbid`.

**Migration safety:** direct-port DSN bypasses PgBouncer for migrations.
The down migration's `REVOKE ... FROM admin_readonly` is guarded to match
the up migration's grant guard — `admin_readonly` is infra-provisioned
ahead of the migration in prod and may not exist in dev/CI.

## 14. Testing strategy

| Suite | Location | Proves |
|---|---|---|
| Unit | `internal/**/*_test.go`, `test/unit/**` | Business logic, hand-written fakes, no I/O |
| Contract | `test/contract` | Wire-shape/error-taxonomy conformance |
| Postgres | `test/postgres` (`-tags=integration`) | Real Postgres via testcontainers-go — RLS-6/RLS-7, real SQLSTATE paths |
| Integration | `test/integration` (`-tags=integration`) | Real OpenBao via testcontainers-go + fake Kubernetes TokenReview server |
| E2E | `test/e2e` (`-tags=e2e`) | Full `cmd/*` composition-root wiring |

`make test-ci` = full parallel merged-coverage pipeline (`-coverpkg`
scoped to `internal/...`/`pkg/...`, `cmd/` deliberately excluded — `main()`
can't be unit-invoked, e2e proves it instead). Currently ~99.4% merged
coverage; remaining gaps are documented-unreachable branches (a
`crypto/rand` failure path, the OpenBao SDK's own 404-swallowing
behavior, etc.) — see `internal/core/service/secret_generator_test.go`
and `internal/adapter/outbound/openbao/client_test.go` for the specific
reasoning on each.

## 18. Integration points

| Service | Relationship |
|---|---|
| Realm Provisioner | Calls TS-4 (register), TS-1 (issue/rotate) — applies the returned secret to Keycloak (the "other half" of every credential transition) |
| Org & Membership / operators | Call TS-3 (read metadata) |
| Org & Membership / Core | Publishes `TenantMembershipsPurged` — this service's one inbound subscription |
| Audit Log | Subscribes to `iam-serviceaccount-events` (5 events, audit-only — no authz consumer, the automation principal carries no roles) |
| Keycloak | Indirect only — runtime validator of the applied secret; this service never calls it (TS-INV-1) |

## 20. Operational considerations

- **Outbox health:** `outbox_pending_total` is the primary bus-health
  signal; sustained growth means the SNS relay is stalled (credential
  operations still succeed — only audit emission is delayed, EVT-4).
- **Stuck `rotating` versions:** `rotation_overlap_active` should trend to
  zero shortly after each rotation's `expires_at`; non-zero beyond one
  sweep interval means `cmd/rotator` isn't running or is failing OpenBao
  deletes.
- **Concurrent-revoke races:** benign by design (LLD TS-D13) — both TS-2
  and the sweep re-check after an optimistic-lock conflict and report the
  already-achieved outcome, not a false failure.

## 21. Performance

Trivially small service by design (§21 of the LLD) — 2 replicas cover HA,
not load; no HPA at MVP. Scaling triggers, if ever needed, would be tenant
count and rotation cadence, both far below any single-replica limit.
