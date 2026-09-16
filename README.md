# iam-token-service

**Custodian of the platform-automation service account's rotating credential material** for the Tender Management SaaS Platform's IAM subsystem — issues, rotates, and revokes the per-tenant `platform-automation` Keycloak client's secret, custodies the plaintext exclusively in OpenBao (never Postgres), and drives the tenant-offboarding credential cleanup cascade.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service`
**Module:** Go 1.26.6 — three binaries (`cmd/server`, `cmd/consumer`, `cmd/rotator`) built from one image, deployed as a Helm chart (Deployment × 2, CronJob × 1)
**Design:** `docs/iam-lld-token-service.md` (rev 1.0, Approved) — this README and `ARCHITECTURE.md` are navigable summaries of it, not a replacement; the LLD is the tie-breaker on any discrepancy.

---

## Mental model

| This service owns | It does NOT own |
|---|---|
| Credential lifecycle (issue / rotate / revoke) for the automation principal | Minting or deleting the Keycloak client itself, or **setting** its secret at Keycloak — the Realm Provisioner is the sole Keycloak Admin API writer (RP-INV-1) |
| The service-account principal registry (metadata only — Keycloak `sub`, `client_id`, type, status, version pointers) | Human-user tokens, JWT issuance, or login — this service never sits on a hot token-issuance path |
| Secret material custody in OpenBao (KV v2, deterministic per-tenant/per-version path) | Authorization decisions for the principal — it carries no roles and is a non-member by construction (O&M AUTH-9) |
| Credential-lifecycle events on `iam-serviceaccount-events` (5 frozen names) via the transactional outbox | Making the principal a member or granting it a role — structurally barred |
| Tenant-offboarding credential cleanup (`TenantMembershipsPurged` → hard-delete + OpenBao purge) | Tenant-owned bots or general user PAT self-service — documented as a post-launch extension of the same tables, not built at MVP |
| Its own `serviceaccount` database — `service_account_principals`, `service_account_credentials`, `outbox_events`, `processed_events` | Anything Keycloak-Admin-shaped — this service has **no `gocloak` dependency at all** (TS-INV-1) |

Two services touch the automation principal's credential, deliberately
split: this service is the credential **system-of-record** (generates,
versions, custodies, tracks status); the **Realm Provisioner** is the sole
**applier** to Keycloak. Neither can complete a rotation alone — see
[Credential issue/rotate flow](ARCHITECTURE.md#credential-issuerotate-flow-ts-1)
for the exact hand-off.

---

## Why this service exists

The automation principal's credential needs a home that is neither
Keycloak (which only the Realm Provisioner may write) nor a general secrets
service with no domain awareness of rotation-overlap, idempotent
issue/rotate semantics, or the offboarding cascade. This service exists to
be that home: a small, single-purpose credential authority with exactly one
external secret-storage dependency (OpenBao) and zero Keycloak Admin API
surface.

---

## API overview

4 routes (TS-1..TS-4) plus health/docs — `internal/adapter/inbound/http/router.go`
is the single source of truth; the generated Swagger spec (`make swag`,
`docs/swagger/`) is derived from `swaggo` annotations on the handlers, never
hand-authored. All routes sit behind `gincommon.DefaultMiddlewares`
(`PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics →
Logging → RequireAuth → ContextMiddleware`) followed by this service's own
`GUCBridgeMiddleware`. There is exactly **one route class** — every route
is under `/api/v1/internal/*`; this service has no public/tenant-facing
surface at MVP.

### Internal routes (4)

| # | Method & path | Purpose |
|---|---|---|
| TS-4 | `POST /api/v1/internal/tenants/:id/service-accounts` | Register the principal after the Realm Provisioner mints the Keycloak client. Idempotent on `(tenant_id, principal_type)`. Emits `ServiceAccountRegistered`. |
| TS-1 | `POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials` | Issue or rotate the credential — the only endpoint that ever returns a plaintext secret, exactly once. Idempotent per `rotation_id`. Emits `…CredentialIssued`/`…CredentialRotated`. |
| TS-2 | `POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials/:version/revoke` | Revoke one credential version immediately; deletes its OpenBao material. Idempotent. Emits `…CredentialRevoked`. |
| TS-3 | `GET /api/v1/internal/tenants/:id/service-accounts/:principal_id` | Read principal + credential **metadata** — never a secret. |

### Health and docs routes

| Method & path | Purpose |
|---|---|
| `GET /healthz` | Liveness (pre-auth) |
| `GET /readyz` | Readiness — DB + OpenBao reachability |
| `GET /swagger/*any` | Rendered OpenAPI (gated, `DOCS_ENABLED`/`DOCS_AUTH_TOKEN`) |
| `GET /asyncapi`, `GET /asyncapi.yaml` | Rendered / raw event contract (gated) |

There is no listing endpoint (`GET .../service-accounts`) at MVP — there is
exactly one automation principal per tenant, read directly by TS-3.

---

## Input validation

Applied in the service layer before any write, so a validation failure
never reaches OpenBao or Postgres: `rotation_id` must be a UUID;
`overlap_seconds` is clamped server-side to `[0, 900]` regardless of what
the caller sends (`0` is a hard cutover); `principal_sub` must be a UUID;
`keycloak_client_id` is validated against the frozen `platform-automation`
value. Failures return `400 invalid_request` with the specific field named
in `details.field`.

### Notable validation rules

- The `:id` path segment must equal the `x-tenant-id` GUC before any DB
  checkout — a mismatch is `403`, never silently corrected.
- The only accepted `x-user-id` on any route is the reserved system
  principal `iam-system` (`00000000-0000-0000-0000-0000000000a1`) — a
  tenant-facing principal is rejected before any handler runs.
- Mutation responses always include `record_version` so callers can
  round-trip the optimistic-concurrency token; no response body ever
  includes a stored secret except TS-1's freshly-generated plaintext.

---

## Architecture

Clean Architecture, structurally enforced by `.go-arch-lint.yml` — `make
lint` fails the build on a layering violation, not just a style warning.
Full diagrams and mechanism-by-mechanism detail live in
[`ARCHITECTURE.md`](ARCHITECTURE.md); this is the summary.

### Dependency rules (enforced by go-arch-lint in CI)

`domain` → `port` → `service` → `{postgres, openbao, observability,
adapters_outbound}` → `{adapters_inbound, reconciler_jobs}` → `cmd`. The one
rule most worth internalizing: **`cmd/rotator` (the `reconciler_jobs`
component) may not depend on `service`** — it drives `postgres`/`openbao`
directly under a `BYPASSRLS` connection for cross-tenant enumeration, a
privilege no HTTP- or event-triggered code path may ever hold.

### Storage and messaging

- **PostgreSQL** (`serviceaccount` database) — `service_account_principals`
  and `service_account_credentials` under `FORCE ROW LEVEL SECURITY`;
  `outbox_events` (owned by `platform-events`) and `processed_events`
  (RLS-exempt, operational).
- **OpenBao** (KV v2) — the *only* place credential plaintext ever lives,
  at the frozen path `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>`.
  Kubernetes-auth only; no static token.
- **AWS SNS/SQS + Glue Schema Registry** — `iam-serviceaccount-events`
  (single producer, 5 frozen event names) via the transactional outbox;
  one inbound subscription (`TenantMembershipsPurged`).

### Shared library dependencies

`platform-pgcommon` (pool/RLS-GUC/retry), `platform-events` (envelope,
outbox, SNS/SQS transport), `platform-gincommon` (HTTP middleware chain,
health/readiness, structured logging), `openbao/openbao/api/v2` (the
OpenBao SDK — Kubernetes auth method only).

---

## Integrating with other services

### 1. Prerequisites

A tenant automation principal must be registered (TS-4) by the **Realm
Provisioner** *after* it mints the `platform-automation` Keycloak client
(RP-1/RP-2) — TS-1 against an unknown `principal_id` is
`404 principal_not_found`.

### 2. Calling TS-4 (register)

The Realm Provisioner calls this once per tenant, supplying the Keycloak
service-account `sub` and `client_id`. Idempotent on
`(tenant_id, principal_type)` — a repeat call returns the existing
principal as `200`, never a duplicate row.

### 3. Calling TS-1 (issue / rotate)

The Realm Provisioner calls this to obtain the plaintext secret it then
applies to the Keycloak client (RP-17); operators and this service's own
rotation CronJob also call it. The caller supplies a `rotation_id` — reuse
the **same** id to safely retry a lost response without generating new
material or a double-emit; use a **different** id only for a genuinely new
rotation (a different one mid-flight is `409 rotation_in_flight`).

### 4. Calling TS-2 (revoke)

Operators, and this service's own offboarding cascade, call this to revoke
one credential version. **This alone does not stop the secret validating
at Keycloak** (TS-INV-7) — pair it with the corresponding Realm Provisioner
action unless the call site is overlap-expiry or offboarding, where a
Keycloak-side change is already implied.

### 5. Calling TS-3 (read)

Org & Membership, operators, and RP-18 call this for credential metadata —
version, status, timestamps, OpenBao *path*. Never returns a secret.

### 6. Subscribing to `iam-serviceaccount-events`

The Audit Log service is this topic's one subscriber
(`serviceaccount-audit-q`, DLQ `serviceaccount-audit-q-dlq`,
`maxReceiveCount=5`). No authorization consumer subscribes — the automation
principal carries no roles, so these events are audit-only. Payloads are
self-contained snapshots (not deltas); dedup on the envelope `id`.

### 7. Publishing `TenantMembershipsPurged`

This service consumes this event from Org & Membership / Core's
tenant-lifecycle topic on `tenant-lifecycle-tokensvc-q` — it does **not**
publish it. It does not consume `MembershipRevoked`; a per-user membership
change never touches a service-account principal.

### 8. Handling errors

Every response uses the flat `platform-gincommon` `ErrorResponse` shape
(`error`, `status`, `trace_id`, `request_id`, `details?`) — see
[Error taxonomy](#security) below and LLD §17 for the full table.

### 9. Rate limits

None enforced at this service — all callers are trusted in-mesh backends
behind a NetworkPolicy allow-list, not external/public traffic.

### 10. Background reconcilers you may observe

`cmd/rotator` runs an overlap-expiry sweep and an orphan-material
reconciler on a cron schedule; both are read-only from any other service's
perspective — they only ever touch this service's own rows and its own
OpenBao paths.

---

## Local development

### Prerequisites

Go 1.26.6, Docker (Compose v2), a `.go_private_token` file (gitignored —
a GitHub PAT with read access to this org's private repos, for the
`go.mod` private-module fetch during image builds; **not** needed for
`make docker-up`, which starts infra only).

### Setup

```bash
make setup        # copies .env-example → .env, installs git hooks
make docker-up     # starts Postgres/PgBouncer/OpenBao/Floci (infra only)
make run           # runs cmd/server natively against that infra
```

`make docker-up` deliberately starts **infrastructure only** (no app
container) — run the service itself natively with `make run` /
`make run-consumer` so you get fast rebuilds and native debugging, matching
the sibling services' convention. The containerized server+consumer image
is available via `make docker-run-app` (requires `.go_private_token`,
since the image build fetches this org's private Go modules), and the
CronJob-equivalent binary via `make docker-run-rotator`.

### Common commands

| Command | Purpose |
|---|---|
| `make build` | Compile all three binaries |
| `make test` | Unit + contract + Postgres + integration suites, in parallel |
| `make test-unit` | Unit tests only, no containers |
| `make lint` | `go-arch-lint` + `golangci-lint` |
| `make gates` | The three invariant gates (`no-gocloak`, `no-secret-log`, `set-local-only`) + `gincommon-obs` |
| `make fmt` / `make vet` | Format / vet |
| `make swag` | Regenerate the Swagger/OpenAPI spec from handler annotations |
| `make godoc` | Serve package documentation locally |
| `make help` | List every target with its one-line description |

### Running a single test

```bash
go test ./internal/core/service/... -run TestCredentialService_IssueOrRotate_Rotate -v
```

### Calling the API locally

```bash
# Register the tenant's automation principal (TS-4)
curl -s -X POST http://localhost:8080/api/v1/internal/tenants/$TENANT_ID/service-accounts \
  -H 'content-type: application/json' \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" \
  -d '{"principal_sub":"<keycloak-sub-uuid>","keycloak_client_id":"platform-automation"}'

# Issue the first credential (TS-1)
curl -s -X POST http://localhost:8080/api/v1/internal/tenants/$TENANT_ID/service-accounts/$PRINCIPAL_ID/credentials \
  -H 'content-type: application/json' \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID" \
  -d '{"rotation_id":"<uuid>","overlap_seconds":300}'

# Read metadata (TS-3)
curl -s http://localhost:8080/api/v1/internal/tenants/$TENANT_ID/service-accounts/$PRINCIPAL_ID \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: $TENANT_ID"
```

### Developer tools

`docker compose up floci-ui` gives a browser view of the local SNS/SQS/Glue
emulator at `http://localhost:4502`; `make cover`/`make cover-func` render
the merged coverage profile; `make pin-base-images` refreshes the
Dockerfile's pinned digests.

---

## Testing domain events locally

Every mutating write publishes a domain event through a **transactional
outbox → SNS → SQS** pipeline, same as the sibling IAM services.

### How the pipeline works

```
HTTP write (TS-1/TS-2/TS-4) or cmd/rotator sweep
    │
    ▼
service layer  ──(same tx)──▶  outbox_events (Postgres)
                                      │
                               outbox runner (OUTBOX_POLL_INTERVAL, 500 ms)
                                      │
                                      ▼
                       SNS: iam-serviceaccount-events   (floci)
                                      │
                    SNS fan-out to serviceaccount-audit-q (local dev only)
```

An outbox insert is atomic with the business write — a `2xx` response
guarantees an `outbox_events` row exists.

### Step 1 — Start infrastructure

```bash
make docker-up
```

`scripts/init-floci.sh` runs automatically and provisions the SNS topic,
the inbound `tenant-lifecycle-tokensvc-q` queue, the outbound
`serviceaccount-audit-q` subscriber queue, and the `iam-serviceaccount-events`
Glue registry + all 5 schemas — floci includes Glue Schema Registry in its
free tier, so `GLUE_REGISTRY_NAME` is set by default in `.env-example` and
the real Glue wire-format codec runs locally instead of falling back to
`NoopCodec`.

### Step 2 — Verify SNS/SQS/Glue exist

```bash
docker compose exec floci aws --region ap-south-1 sns list-topics
docker compose exec floci aws --region ap-south-1 sqs list-queues
docker compose exec floci aws --region ap-south-1 glue list-schemas --registry-id RegistryName=iam-serviceaccount-events
```

### Step 2b — Verify event delivery in the browser (floci-ui)

`make docker-up` also starts **floci-ui**, a web console for floci, at
**http://localhost:4502**. It's a faster way to confirm an event landed on
the right queue than shelling into the CLI each time:

1. Open **http://localhost:4502** → sidebar → **Integration → SQS**. You'll
   see `serviceaccount-audit-q` and `tenant-lifecycle-tokensvc-q`, each with
   a **Messages** column.
2. Trigger an event — call TS-1 (issue/rotate) or TS-2 (revoke) per the
   curl examples above.
3. Refresh the SQS list. The **Messages** count on `serviceaccount-audit-q`
   should go up by one — this registry is single-producer/single-topic
   (§25), so every published event fans out to that one catch-all queue,
   unlike a sibling service's per-`EventType` filter policies.

### Step 2c — Retrieve the event body (CLI)

```bash
# Peek without deleting — the message stays and becomes visible again after
# the queue's VisibilityTimeout (30s by default).
docker compose exec floci aws --region ap-south-1 sqs receive-message \
  --queue-url http://floci:4566/000000000000/serviceaccount-audit-q \
  --max-number-of-messages 10 --message-attribute-names All
```

Or skip SQS entirely and read the outbox table directly — fastest during
dev, and shows the plain-JSON payload before the Glue wire-format header is
prepended at publish time:

```bash
docker compose exec postgres psql -U serviceaccount_app -d serviceaccount -c \
  "SELECT event_type, jsonb_pretty(payload::jsonb) FROM outbox_events ORDER BY created_at DESC LIMIT 3;"
```

### Step 3 — Trigger an event and inspect the outbox

```bash
make run
# ... issue a TS-1/TS-2/TS-4 call (see "Calling the API locally" above) ...

docker compose exec postgres psql -U serviceaccount_app -d serviceaccount -c \
  "SELECT id, event_type, published_at IS NOT NULL AS published, attempts
   FROM outbox_events ORDER BY created_at DESC LIMIT 20;"
```

### Troubleshooting events

| Symptom | Likely cause | Fix |
|---|---|---|
| `outbox_events` row never gets `published_at` set | `SNS_TOPIC_SERVICEACCOUNT_ARN` mismatch, or outbox runner not started | Re-check `.env` against `aws --region ap-south-1 sns list-topics` (or floci-ui at http://localhost:4502) |
| `outbox_events` empty after a write | Row was published and pruned, or the write never committed | Re-check the HTTP response code — a `2xx` guarantees the row was committed |
| Messages keep reappearing after `receive-message` | Normal — SQS visibility timeout, not deletion | Use `delete-message` |
| `floci` never reports healthy | Its healthcheck specifically waits for the `ServiceAccountRevoked` Glue schema to exist (the last resource `init-floci.sh` creates), not just SNS reachability | Check `docker compose logs floci` — a slow init looks like a stuck healthcheck rather than a real failure for the first ~10–20s |

---

## Testing

### Canonical tests (do not break)

- `test/postgres` — real Postgres via testcontainers-go, proving
  `FORCE ROW LEVEL SECURITY` actually blocks cross-tenant access (not just
  that application code happens to filter correctly).
- `test/integration` — real OpenBao via testcontainers-go with a fake
  Kubernetes TokenReview server, proving the Kubernetes-auth login path
  end-to-end (this cannot be faked with a static-token shortcut).
- `test/unit/consumer` — the offboarding cascade's ack-vs-retry semantics
  (missing/invalid envelope id, unknown event type → ack, never DLQ-storm).

### Coverage

`make test-ci` runs the full parallel suite with `-coverpkg` scoped to
`internal/...`/`pkg/...` (deliberately excluding `cmd/` — a `main()` can
never be unit-invoked; `test/e2e` proves it works instead), merges all
profiles with a max-count strategy, and enforces the CI coverage floor in
`.github/workflows/validate-test.yml`.

---

## Environment variables

| Variable | Default (dev) | Purpose |
|---|---|---|
| `APP_ENV`, `APP_NAME`, `APP_PORT` | `dev`, `iam-token-service`, `8080` | Environment / API port |
| `METRICS_PORT` | `9090` | Dedicated `/metrics` listener, separate from `APP_PORT` |
| `LOG_LEVEL` | `debug` | `slog` level |
| `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` | — | `platform-pgcommon` DB config (app pool) |
| `PG_MAX_CONNS`/`PG_MIN_CONNS`/`PG_BOUNCER_MODE`/`PG_STATEMENT_TIMEOUT` | `20`/`0`/`true`/unset | Pool sizing and PgBouncer transaction-pooling mode |
| `MIGRATION_DATABASE_URL` | direct port | Bypasses PgBouncer for migrations (`pg_advisory_lock` is session-scoped) |
| `RECONCILER_DATABASE_URL` | direct port | `serviceaccount_reconciler` (`BYPASSRLS`, `SELECT`-only) pool for `cmd/rotator` |
| `OPENBAO_ADDR`, `OPENBAO_ROLE`, `OPENBAO_KV_MOUNT` | `http://localhost:8210`, `iam-token-service`, `iam` | OpenBao Kubernetes-auth login + KV v2 mount |
| `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL` | `ap-south-1`, `test`/`test`, Floci endpoint | AWS SDK config (Floci locally, real AWS in deployed environments) |
| `GLUE_REGISTRY_NAME` | `iam-serviceaccount-events` | Glue Schema Registry name |
| `SNS_TOPIC_SERVICEACCOUNT_ARN` (or `SNS_TOPIC_ARN` alias) | — | Produced-event topic |
| `OUTBOX_POLL_INTERVAL`, `OUTBOX_BATCH_SIZE`, `OUTBOX_MAX_ATTEMPTS`, `OUTBOX_DRAIN_TIMEOUT`, `OUTBOX_PUBLISH_CONCURRENCY`, `OUTBOX_PUBLISH_TIMEOUT`, `OUTBOX_STARTUP_JITTER`, `OUTBOX_CLAIM_LEASE_DURATION` | see `.env-example` | `platform-events` outbox runner tuning |
| `SQS_OFFBOARDING_QUEUE_URL`, `SQS_OFFBOARDING_CONCURRENCY`, `SQS_MAX_MESSAGES`, `SQS_WAIT_SECONDS`, `SQS_VISIBILITY_TIMEOUT` | see `.env-example` | Offboarding consumer's one inbound subscription |
| `ROTATION_DEFAULT_OVERLAP_SECONDS`, `ROTATION_DEFAULT_CADENCE_DAYS` | `300`, `90` | Rotation-overlap tuning (`overlap_seconds` is still clamped to `[0,900]` server-side regardless) |
| `PROCESSED_EVENTS_TTL_DAYS` | `8` | Dedup-ledger retention (must exceed SQS's own message lifetime) |
| `DOCS_ENABLED`, `DOCS_AUTH_TOKEN` | `true`, empty | Swagger/AsyncAPI viewer gating (always open outside `production`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | OTLP export target; unset in dev still yields valid in-process trace IDs, just no export |

See `.env-example` for the complete, currently-accurate list with inline
comments — this table is a curated summary, not a substitute.

---

## Security

| Topic | Guidance |
|---|---|
| Tenant isolation | `FORCE ROW LEVEL SECURITY` on both tenant-scoped tables; the policy predicate fails closed (no GUC → zero rows / write rejected) |
| GUC scoping | `SET LOCAL app.tenant_id`, transaction-local only — never a session-wide `SET`, which would leak across a pooled connection |
| Cross-tenant enumeration role | `serviceaccount_reconciler` — `BYPASSRLS` but grantless beyond `SELECT`; used only by `cmd/rotator` |
| Privilege separation | Three distinct roles (`serviceaccount_app`/`_migrator`/`_reconciler`), each scoped to exactly what its call sites need |
| No Keycloak credential | This service has no `gocloak` dependency and no Keycloak Admin credential anywhere (TS-INV-1, CI-enforced) |
| Secret-in-Postgres / secret-in-logs | Structurally impossible by design (TS-INV-2) — the CI `no-secret-log` gate fails the build if a credential field name reaches a log/trace sink |
| OpenBao authentication | Kubernetes auth method only (pod ServiceAccount → role → policy); no static token, no AWS Secrets Manager permission |
| OpenBao path scope | ACL policy grants only `iam/{data,metadata}/serviceaccount/*` — nothing wider |
| Break-glass revoke | TS-2 alone does not invalidate a live secret at Keycloak (TS-INV-7) — pair with the Realm Provisioner action |
| NetworkPolicy ingress | `networkPolicy.ingressNamespaceSelector` has no safe default — the Helm render fails closed if left unset rather than falling back to an unrestricted `{}` selector |
| Go-code SAST | `gosec` (`make sast`), distinct from `govulncheck` (dependency CVEs) and the release pipeline's Trivy scan (container/OS CVEs) |

---

## Observability

SLOs are availability-oriented, not latency-critical — this service is
never on the hot token-issuance path (Keycloak validates the secret at
runtime, not this service). Internal API availability target: 99.9%
monthly.

### Metrics

Follows the **Enterprise Platform Observability Standard**'s three-tier
taxonomy:

- **Tier 1** (`platform_*`) — semantics shared across every domain
  (IAM/Workflow/Billing/...). Required labels: `domain`, `service`,
  `environment`.
- **Tier 2** (`iam_*`) — semantics shared across IAM services only.
  Required labels: `service`, `environment`.
- **Tier 3** (`iam_token_service_*`, frozen §25) — this service's own
  business counters: `credentials_issued_total`, `rotation_overlap_active`,
  `openbao_call_duration_seconds`, `offboarding_cascade_total`,
  `rotation_sweep_total`, `material_reconcile_total`,
  `processed_events_duplicates_total`, `unknown_event_acknowledged_total`,
  `outbox_pending`.

Tier-1/Tier-2 labels are injected **centrally**
(`metrics.Register(environment)`), never at the instrumentation call site,
so they can't be omitted or misspelled — see
[ARCHITECTURE.md § Observability stack](ARCHITECTURE.md#observability-stack)
for the full table and [`docs/observability-registry-proposals.md`](docs/observability-registry-proposals.md)
for this service's proposed `platform_dependency_request_seconds`,
`platform_duplicate_messages_total`, and `iam_offboarding_cascade_total`
metrics (dual-emitted alongside their legacy Tier-3 equivalents pending
registry ratification). `make gates` includes a `metrics-taxonomy` check
enforcing naming/namespace compliance.

Shared-library metrics (`http_request_duration_seconds`/`http_requests_total`
from `platform-gincommon`; `events_*`/`outbox_*`/`sqs_*` from
`platform-events`; `pgcommon_pool_*` from `platform-pgcommon`) predate this
Standard and carry a `service` label that combines domain+service
(`"iam-token-service"`) rather than the Standard's separate `domain`/
`service` labels — a known gap this service cannot fix unilaterally (it
would require a change in each shared library).

### Tracing and logs

OTel spans `credential.issue_rotate` and `credential.revoke` (attrs
`tenant_id`, `principal_id`, `version`, `op`), propagated onto the produced
event envelope's `trace_id` so a rotation is traceable end-to-end.
Structured `slog` JSON logs carry `tenant_id`/`principal_id`/`version`/`op`/
`result` — never a credential field.

---

## Deployment

### Container image — three binaries

| Binary | Entrypoint | Purpose |
|---|---|---|
| `cmd/server` | `/iam-token-service` (default) | HTTP API (TS-1..TS-4) + outbox runner |
| `cmd/consumer` | `/iam-token-service-consumer` | Offboarding SQS subscriber |
| `cmd/rotator` | `/iam-token-service-rotator` | Overlap-expiry sweep + orphan-material reconciler + prune, run-to-completion |

### Helm chart

`deploy/helm/` — `Deployment` × 2 (server, consumer, each `replicaCount: 2`
for HA, not load), `CronJob` × 1 (rotator — `activeDeadlineSeconds: 240s`,
deliberately shorter than its 5-minute schedule so a run never eats into
the next scheduled tick under `concurrencyPolicy: Forbid`), `NetworkPolicy`
restricting ingress to in-mesh callers (`networkPolicy.ingressNamespaceSelector`
is required — the render fails if left unset), `ServiceAccount` bound to
the OpenBao Kubernetes-auth role, `PrometheusRule` + `ServiceMonitor`,
`PodDisruptionBudget` (`minAvailable: 1`).

HPA (`autoscaling.enabled`, 2–8 replicas on CPU 70%/memory 75%) and
Ingress/HTTPRoute/SecurityPolicy (`ingress.enabled`) both ship as
disabled-by-default scaffolding, matching the sibling IAM services'
chart shape — this service has no public surface today (all routes are
`/api/v1/internal/*`) and a fixed `replicaCount` is the current scaling
model, so neither actually renders unless explicitly turned on.

### Migration safety

`MIGRATION_DATABASE_URL` connects directly (bypassing PgBouncer) as
`serviceaccount_migrator` (`BYPASSRLS` + DDL) — `pg_advisory_lock` is
session-scoped and breaks under transaction pooling. Nothing has been
deployed to any environment yet, so migrations are outright (no
expand/contract dance) — there is exactly one so far.

---

## CI

Nine workflow files:

- **`ci.yml`** — orchestrator. Runs `validate-test.yml` and
  `validate-quality.yml` in parallel with `build-image` (Buildx cached
  build → Trivy CVE scan → smoke tests). On push to `main`: builds+pushes
  to GHCR.
- **`validate-test.yml`** (reusable) — `make test-ci` (unit + contract +
  Postgres/RLS + integration, `-race`, merged coverage) → coverage
  threshold gate (**98%**) → `go-arch-lint` → Swagger staleness check →
  event-schema sync check → `test-e2e`.
- **`validate-quality.yml`** (reusable) — `go mod verify` → `gofmt` check →
  `go mod tidy` drift check → `go vet` → `golangci-lint` → the invariant
  gates (`no-gocloak`/TS-INV-1, `no-secret-log`/TS-INV-2,
  `set-local-only`/RLS-6, `gincommon-obs`, `metrics-taxonomy`) →
  `govulncheck` → `gosec` (SAST).
- **`changelog-check.yml`** — requires a `CHANGELOG.md` entry on every PR
  touching runtime behavior.
- **`release.yml`** — tag-triggered release pipeline: re-validate →
  build+cross-compile → Docker build/push/sign → GitHub Release publish.
- **`schema-registry.yml`** — registers this service's 5 event schemas to
  the single-producer `iam-serviceaccount-events` Glue registry: PR
  read-only validate+diff, push-to-`main` full
  validate→diff→register→changelog.
- **`schema-prune.yml`** — monthly dry-run orphan-schema report plus an
  operator execute path.
- **`schema-health-quarterly.yml`** — read-only quarterly
  lifecycle-annotation lint + Glue version-accumulation scan.
- **`freeze-watchdog.yml`** — daily cron alerting when a schema-registry
  `SCHEMA_FREEZE` has been left active too long.

**Required GitHub Actions repository secret: `GO_PRIVATE_TOKEN`** — every
workflow that runs `go mod download`, or installs `schema-gov` from source
(the private `platform-schemagov` repo), needs it.

---

## Docker

### What the bundled docker-compose.yml starts

`postgres` + `pgbouncer` (RLS-scoped app role, migrator role, reconciler
role all provisioned by `scripts/init-db.sql`), `openbao` (dev-mode — real
Kubernetes auth cannot succeed against it locally; seed by hand or use
`make test-integration`), `floci` + `floci-ui` (SNS/SQS/Glue emulator).
`server`/`consumer`/`rotator` app containers are also defined in
`docker-compose.yml` but **not** part of the default `make docker-up`
(infra-only) — start them with `make docker-run-app` /
`make docker-run-rotator`, or use the native `make run` path.

### Building the service image

```bash
make docker-run-app   # requires .go_private_token — build + run server & consumer
# or, directly:
docker build -t iam-token-service --secret id=go_private_token,src=.go_private_token .
```

### Health and readiness

`GET /healthz` (liveness, pre-auth) and `GET /readyz` (readiness — checks
DB + OpenBao reachability) are registered before the auth middleware.

### Minimum required environment variables

```bash
PG_HOST=localhost PG_PORT=5536 PG_USER=serviceaccount_app PG_PASSWORD=devpassword PG_DBNAME=serviceaccount
OPENBAO_ADDR=http://localhost:8210 OPENBAO_ROLE=iam-token-service OPENBAO_KV_MOUNT=iam
AWS_REGION=ap-south-1 AWS_ENDPOINT_URL=http://localhost:4568 GLUE_REGISTRY_NAME=iam-serviceaccount-events
```

---

## Cross-service dependencies

| Direction | Service | Relationship |
|---|---|---|
| Called by | Realm Provisioner | TS-4 (register), TS-1 (issue/rotate) — RP applies the returned secret to Keycloak |
| Called by | Org & Membership, operators | TS-3 (read metadata) |
| Called by | Operators, this service's own rotation CronJob | TS-1 (rotate), TS-2 (revoke) |
| Consumes from | Org & Membership / Core | `TenantMembershipsPurged` (tenant-lifecycle topic) |
| Publishes to | Audit Log | 5 events on `iam-serviceaccount-events` |
| Never calls | Keycloak Admin API | TS-INV-1 — zero `gocloak` dependency |

---

## Out of scope

- Human-user tokens, JWT issuance, or any login path.
- Authorization decisions or role/membership grants for the automation
  principal.
- Setting the Keycloak client secret at Keycloak itself (Realm Provisioner
  only).
- Tenant-owned bots or user PAT self-service (documented Phase-2 extension
  of the same schema, not built).
- A public/tenant-facing API surface (MVP is internal-mesh-only).

---

## Contributing

Open a PR against `main` — CI must pass (`make lint`, `make gates`,
`make test-ci`) and `CHANGELOG.md` must be updated for any
runtime-behavior change. There is no separate `CONTRIBUTING.md` in this
repo yet; the tables below are the closest thing to a contributor guide.

| Document | Description |
|---|---|
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) | Top-level guidance for Claude Code working in this repo |
| [`.claude/architecture.md`](.claude/architecture.md) | Package layout, `.go-arch-lint.yml` dependency rules |
| [`.claude/database-schema.md`](.claude/database-schema.md) | Tables, RLS, triggers, invariants |
| [`.claude/api-events.md`](.claude/api-events.md) | Endpoint catalogue, event contract |
| [`.claude/request-flows.md`](.claude/request-flows.md) | Per-flow walkthroughs, concurrency, failure handling |
| [`.claude/operations.md`](.claude/operations.md) | Security, observability, config, deployment, testing |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Detailed architecture narrative with diagrams |
| [`docs/iam-lld-token-service.md`](docs/iam-lld-token-service.md) | Full LLD (rev 1.0, Approved) — §16 open questions, §17 error taxonomy, §25 frozen name inventory |
| [`docs/observability-registry-proposals.md`](docs/observability-registry-proposals.md) | Tier-1/Tier-2 metric registry submissions pending ratification |

---

## License / ownership

Internal service, owned by the BCBP Solutions IAM/Platform team
(`platform@bcbpsolutions.com`). Not for external distribution.
