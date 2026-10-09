# iam-token-service

**Custodian of the platform-automation service account's rotating credential material** for the Tender Management SaaS Platform's IAM subsystem. It issues, rotates and revokes the per-tenant `platform-automation` Keycloak client's RSA-2048 signing keys, keeps the private-key plaintext only in OpenBao (never Postgres), serves the public half to Keycloak as a JWK Set, and runs the tenant-offboarding credential cleanup cascade.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service`
**Module:** Go 1.26.9. Four binaries (`cmd/server`, `cmd/consumer`, `cmd/rotator`, `cmd/scheduler`) built into one distroless image and deployed by one Helm chart (2 Deployments, 2 CronJobs, 1 migrate hook Job).
**Design:** [`docs/lld/iam-lld-token-service.md`](docs/lld/iam-lld-token-service.md) (rev 1.4, Approved). This README and `ARCHITECTURE.md` summarise it. The LLD wins on any discrepancy; the recent hardening passes are recorded there as §22 TS-D21, TS-D22 and TS-D23.

---

## Mental model

| This service owns | It does NOT own |
|---|---|
| Credential lifecycle (issue / rotate / revoke) for the automation principal | Minting or deleting the Keycloak client, or configuring it at Keycloak. The Realm Provisioner is the sole Keycloak Admin API writer (RP-INV-1) |
| The service-account principal registry (metadata only: Keycloak `sub`, `client_id`, type, status, version pointers) | Human-user tokens, JWT issuance or login. This service is never on a token-issuance hot path |
| Key custody in OpenBao (KV v2, deterministic per-tenant/per-version path) and the public JWKS Keycloak fetches (EXT-6) | Authorization decisions for the principal. It carries no roles and is a non-member by construction (O&M AUTH-9) |
| Credential-lifecycle events on `iam-serviceaccount-events` (5 frozen names) via the transactional outbox | Making the principal a member or granting it a role (structurally barred) |
| Tenant-offboarding credential cleanup (`TenantMembershipsPurged` → hard-delete + OpenBao purge) | Tenant-owned bots or user PAT self-service (a documented post-launch extension of the same tables, not built) |
| Its own `serviceaccount` database: `service_account_principals`, `service_account_credentials`, `outbox_events`, `processed_events`, `rls_violation_log`, `keys_refresh_pending` | Anything Keycloak-Admin-shaped. There is **no `gocloak` dependency at all** (TS-INV-1) |

Two services touch the automation credential, deliberately split. This service is the **system of record**: it generates, versions and custodies the keys, tracks their status and serves the public keys. The **Realm Provisioner** is the only **applier** to Keycloak: after every rotate or revoke someone must call its RP-17 (`ClearServiceAccountKeysCache`) so Keycloak drops its cached JWKS and re-fetches it. Keycloak has no cache TTL that would do this on its own (TS-INV-7). See [Credential issue/rotate flow](ARCHITECTURE.md#credential-issuerotate-flow-ts-1).

**Write ordering, the same in every flow: material first, then the Postgres commit, then RP-17.**
1. TS-1, TS-2, the overlap sweep and the offboarding cascade each run in one Postgres transaction that first locks the principal row (`SELECT … FOR UPDATE`). The OpenBao write or delete happens inside that transaction, before the commit (TS-1/TS-2 bound it at 5s, so a hung OpenBao call cannot hold the lock indefinitely). If it fails, nothing commits; an HTTP caller gets `502 secret_store_unavailable`. There is no "record intent, reconcile later" path: every write here is fail-closed.
2. The commit writes the row change and the outbox event atomically.
3. RP-17 runs strictly after the commit, never inside it. It is the one step that cannot be rolled back, so a failure there leaves no inconsistent local row, only an owed refresh. The rotator and scheduler record that debt durably in `keys_refresh_pending` and retry it on every run (TS-D22/TS-D23).

See [Write ordering discipline](ARCHITECTURE.md#write-ordering-discipline) for the sequence diagram.

---

## Why this service exists

- **One holder of the credential-lifecycle logic.** Issue/rotate/revoke idempotency, overlap-window math and the offboarding cascade live in one codebase instead of being re-derived by every caller that would otherwise talk to OpenBao directly.
- **A structural split from Keycloak administration.** If this service could also write to Keycloak, one compromise would both custody and apply material. TS-INV-1 keeps that impossible: `.github/scripts/check-no-gocloak.sh` (`make gates`) fails the build on any `Nerzal/gocloak` import.
- **Plaintext custody with one exit door.** TS-INV-2 (no secret plaintext in Postgres) and the CI `no-secret-log` gate mean a private key exists outside OpenBao only in the one TS-1 response body that returns it.
- **A stable contract for every caller.** The Realm Provisioner, Org & Membership, Workflow and operator tooling integrate against this service's HTTP/event contract, not OpenBao's KV API or Keycloak's client-jwt configuration.

---

## What runs

All four binaries ship in one image (`/iam-token-service-server` is the default `ENTRYPOINT`).

| Binary | Entrypoint | Kind | What it does |
|---|---|---|---|
| `cmd/server` | `/iam-token-service-server` | Deployment | HTTP API (the 7 routes below) and the outbox relay to SNS. Background loops: the rotation-overlap exporter, the RLS-violation exporter, the `keys_refresh_pending` exporter and the JWKS known-tenant refresher. With `MIGRATE_ONLY=true` it migrates and exits (the Helm migrate Job) |
| `cmd/consumer` | `/iam-token-service-consumer` | Deployment | SQS consumer for `TenantMembershipsPurged`, the offboarding cascade |
| `cmd/rotator` | `/iam-token-service-rotator` | CronJob (`*/5 * * * *`) | Retries owed RP-17 refreshes, then the overlap-expiry sweep (revoke + RP-17 per tenant), the orphan-material reconciler and the retention prunes. Never rotates |
| `cmd/scheduler` | `/iam-token-service-scheduler` | CronJob (`*/5 * * * *`) | Retries owed RP-17 refreshes, then cadence-driven rotation: calls `CredentialService.IssueOrRotate` in-process for every principal past `next_rotation_at`, then RP-17 (§16 TSQ-6, TS-D14) |
| migrate Job | `/iam-token-service-server` | Helm pre-install/pre-upgrade hook | Server image with `MIGRATE_ONLY=true`. Every other workload runs with `RUN_MIGRATIONS=false` and never holds the migrator DSN |

The two CronJobs process their batch (`*_BATCH_LIMIT`, default 500) **tenant by tenant**: each tenant's RP-17 runs inline after its rows, on a detached context with a 10s timeout, and a tenant starts only if 30s of run budget remain. On SIGTERM they finish the current tenant and stop. The owed-refresh retry pass at the start of each run uses at most half the budget. Exit code: 0 for success (rows left over as `Deferred` for lack of time included), 1 for any `Failed` result.

---

## API overview

Seven routes, all registered in `internal/adapter/inbound/http/router.go` (`NewRouter`), plus health and docs. The Swagger spec in `docs/swagger/` is generated from handler annotations (`make swag`), never hand-written. Full contracts: LLD §5 and [`.claude/api-events.md`](.claude/api-events.md).

| # | Method & path (under `/api/v1/internal/tenants/:id`) | Idempotency | Purpose | Emits |
|---|---|---|---|---|
| TS-4 | `POST /service-accounts` | On `(tenant_id, principal_type)`: a repeat returns `200` with the existing row; a changed `principal_sub`/`keycloak_client_id` (an RP-3/RP-4 re-mint) updates it in place | Register the principal after the Realm Provisioner mints the Keycloak client | `ServiceAccountRegistered` |
| TS-1 | `POST /service-accounts/:principal_id/credentials` | Body field `rotation_id`: a replay within `ROTATION_REPLAY_WINDOW` returns the same key (`200`) | Issue (v1) or rotate to a new keypair. The only route that returns a private key | `…CredentialIssued` / `…CredentialRotated` |
| TS-2 | `POST /service-accounts/:principal_id/credentials/:version/revoke` | Revoking an already-revoked version is a no-op success | Revoke one version now and delete its OpenBao material | `…CredentialRevoked` |
| TS-3 | `GET /service-accounts/:principal_id` | Read-only | Principal + credential **metadata** (never a key) | — |
| TS-5 | `GET /service-accounts?principal_sub=<uuid>` | Read-only | Find a principal by Keycloak `sub` (AUTH-9 defense-in-depth for org-membership, TS-D16). Identity/status only | — |
| TS-6 | `GET /service-accounts/platform-automation` | Read-only | Read the tenant's automation principal including its `sub` (TS-D17), for the Workflow Service's connector workers. The sub is stable across rotation and changes on an RP-3/RP-4 re-mint; `principal_id` is the stable handle | — |
| EXT-6 | `GET /service-accounts/platform-automation/jwks.json` | Read-only, **public** | The JWK Set Keycloak's `client-jwt` authenticator fetches | — |

### Auth model

- **TS-1..TS-6** accept exactly one caller identity: `x-user-id` = the reserved iam-system principal `00000000-0000-0000-0000-0000000000a1`, and `x-tenant-id` = the path tenant. There is no operator-vs-system distinction. Which caller may reach which route is enforced at the mesh: NetworkPolicy (L4) plus the Istio `AuthorizationPolicy` (L7), which is required outside dev (see [Security](#security)).
- **Middleware chain:** 1 MB body cap → `gincommon.ObservabilityMiddlewares` (ends with the 30s request timeout, so metrics, logs and spans record the same `503` the client gets) → on the protected group only: `RequireIdentityHeaders` (`401 missing_identity_headers` for a missing, repeated or non-UUID `x-user-id`/`x-tenant-id`; it also drops `x-tenant-roles`, which this service never reads) → `gincommon.ProtectedMiddlewares` → `GUCBridgeMiddleware` (rejects any principal other than iam-system, binds `SET LOCAL app.tenant_id`) → `RequireJSONContentType` → `RequireTenantPathMatch` (`403 tenant_path_mismatch`, never silently corrected).
- **The JWKS route** has no header auth by design: Keycloak's outbound fetch carries no caller identity. RLS is bound from the path tenant instead. It is protected by rate limits and, in deployed environments, by the `AuthorizationPolicy` that admits Keycloak to this one path only.
  - **Known-tenant set:** reloaded from Postgres every `JWKS_KNOWN_TENANTS_REFRESH` (15s) over the BYPASSRLS reconciler pool. The pod blocks `/readyz` until the first reload completes (TS-RP-GAP-001), ensuring a newly-provisioned tenant's first Keycloak JWKS fetch is served from the global bucket, not the tighter unknown-tenant one. Tracked by `iam_token_service_jwks_known_tenants_refresh_total{result=success|error}` and `iam_token_service_jwks_known_tenants_last_refresh_age_seconds`. The alert `IAMTokenServiceJWKSKnownTenantsStale` fires when age > 120 s for 5 minutes.
  - Rate limits: every request spends its per-tenant bucket (`JWKS_RATE_LIMIT_PER_TENANT_*`, 5 rps / burst 10, bounded LRU) plus a shared bucket. **Known** tenants (reloaded set plus tenants served since the last reload) spend the global bucket (20/40). **Unknown** tenants spend only the unknown-tenant bucket (2/5), so a flood of random tenant ids cannot starve Keycloak's real fetches. A refusal is `429 rate_limited`.
  - Responses: `200` with the active and in-overlap keys (expired overlap keys are skipped). `503 jwks_keys_unavailable` when the active key, or every key, cannot be read from OpenBao, so Keycloak keeps its cached keys rather than caching a set without the current key. A set missing only rotating keys is still `200`, counted in `iam_token_service_jwks_key_errors_total`. Headers: `Cache-Control: no-cache`, `X-Content-Type-Options: nosniff`.

### Idempotency and replay

TS-1's idempotency key is a **body field**, `rotation_id`, not an `Idempotency-Key` header, because it identifies which rotation this is. A replay of the same `rotation_id` within `ROTATION_REPLAY_WINDOW` (default 15m, `0` = unlimited) after issue returns the same key with `200`. Past the window it is `409 credential_replay_expired`; once that version is revoked it is `409 credential_replay_revoked`. Every replay is logged and counted (`iam_token_service_credential_replays_total{result}`). TS-1 responses carry `Cache-Control: no-store`.

### Concurrency

TS-1, TS-2 and the offboarding cascade serialize per principal on a row lock (`LockForUpdate`; offboarding uses `LockByTenant`). A caller that cannot get the lock within `PG_LOCK_TIMEOUT` (SQLSTATE `55P03`) gets `409 rotation_in_flight`, with `details.active_rotation_id` when it can be looked up. TS-1 takes the next version as `MaxVersion + 1` over every row (a version number is never reused), generates the keypair before the transaction, closes any older open overlaps, and after commit runs an opportunistic sweep of expired overlaps. The JWKS route reads without locking. Details: LLD §9, [`.claude/request-flows.md`](.claude/request-flows.md).

### Error codes

Every non-2xx response is the flat `platform-gincommon` envelope `{error, status, trace_id, request_id, details?}` (`internal/adapter/inbound/http/errors.go`). A bind failure is one `400 invalid_request` (fail-fast, single error; `details.field` names the field).

| Code | Status | Trigger | §17 |
|---|---|---|---|
| `invalid_request` | 400 | Malformed JSON, a UUID/integer parse failure, or a business-rule field check | Frozen |
| `missing_identity_headers` | 401 | `x-user-id`/`x-tenant-id` missing, repeated or not a UUID, or `x-user-id` is not iam-system | Frozen |
| `tenant_path_mismatch` | 403 | Path `:id` differs from `x-tenant-id` | Additive |
| `principal_not_found` | 404 | Unknown `principal_id` (or no principal for TS-5/TS-6) | Frozen |
| `rotation_in_flight` | 409 | Principal row lock not obtained within `PG_LOCK_TIMEOUT` (`details.active_rotation_id`, best effort) | Frozen |
| `optimistic_lock_conflict` | 409 | A concurrent writer changed `record_version` (`details.expected_version`) | Frozen |
| `credential_replay_revoked` | 409 | `rotation_id` replay of a since-revoked version (TS-D13) | Additive |
| `credential_replay_expired` | 409 | `rotation_id` replay past `ROTATION_REPLAY_WINDOW` (`details.version`) | Additive |
| `unsupported_media_type` | 415 | Write body without `Content-Type: application/json` | Additive |
| `principal_revoked` | 422 | TS-1/TS-2 against a revoked (offboarded) principal | Frozen |
| `rate_limited` | 429 | JWKS route limiter refused the request | Additive |
| `internal_error` | 500 | Anything unclassified (logged) | Additive |
| `secret_store_unavailable` | 502 | OpenBao write/delete failed; nothing was committed | Frozen |
| `db_unavailable` | 503 | Postgres connectivity/resource exhaustion, by SQLSTATE class `08`/`53`/`57`/`58` or a closed pool only (TS-D16) | Additive |
| `jwks_keys_unavailable` | 503 | JWKS route: the active key, or every key, unreadable (TS-D23) | Additive |

The seven frozen codes are LLD §17 and cannot change in place; the domain-level additive codes are commented as such in `internal/core/domain/errors.go`.

### Notable validation rules

| Field | Rule | Error |
|---|---|---|
| `id` (path) | UUID; must equal `x-tenant-id` | `400` / `403 tenant_path_mismatch` |
| `principal_id` (path), `principal_sub` (TS-4 body, TS-5 query) | UUID | `400 invalid_request` |
| `rotation_id` (TS-1 body) | Required UUID (checked in the service layer) | `400 invalid_request` |
| `overlap_seconds` (TS-1 body) | Omitted = `ROTATION_DEFAULT_OVERLAP_SECONDS` (300); `0` = hard cutover; clamped to `[0, 900]` (`domain.ClampOverlapSeconds`) | Never rejected |
| `version` (path, TS-2) | Integer | `400 invalid_request` |
| `keycloak_client_id` (TS-4 body) | `platform-automation` or `platform-automation-<tenant_id>` (`domain.ValidPlatformAutomationClientID`, service layer) | `400 invalid_request` |

Mutation responses include `record_version`. No response ever includes a stored secret except TS-1's freshly generated private key. Logs carry `tenant_id`, `principal_id`, `version`, `op`, `result`, never a credential field.

### Health and docs routes

| Route | Purpose |
|---|---|
| `GET /healthz` | Liveness (pre-auth) |
| `GET /readyz` | Readiness: database, OpenBao (a real Kubernetes-auth login), outbox runner, and JWKS known-tenant refresher (TS-RP-GAP-001 — pod stays not-ready until the first DB query populates the known-tenant set), each with a 2s deadline. Returns 503 while draining after SIGTERM |
| `GET /swagger/*any`, `GET /asyncapi`, `GET /asyncapi.yaml` | Docs. Always mounted in `local`/`dev`/`test`. In every other `APP_ENV` (staging included) mounted only when `DOCS_ENABLED=true`, and then bearer-locked by `DOCS_AUTH_TOKEN`; startup fails if it is missing |

`/metrics` is served on its own listener (`METRICS_PORT`), never on the API port.

---

## Architecture

Clean Architecture: `domain` ← `port` ← `service` ← adapters ← `cmd`, enforced by `.go-arch-lint.yml` (`make arch-lint`, also in `make ci` and `validate-test.yml`). Full diagrams: [`ARCHITECTURE.md`](ARCHITECTURE.md); terse layout reference: [`.claude/architecture.md`](.claude/architecture.md).

```
iam-token-service/
├── cmd/
│   ├── server/          # main.go, wiring.go (outbox), exporters.go (overlap gauge, iam_rls_violations_total, keys_refresh_pending), swagger_info.go
│   ├── consumer/        # main.go, inbound_schema.go (consumed-schema validation), dlq.go (DLQ router)
│   ├── rotator/         # sweep.go, orphan_reconciler.go, prune.go, keys_refresh.go (owed RP-17 retry) — BYPASSRLS, no `service` dependency (RLS-7)
│   └── scheduler/       # scan.go (cadence rotation), keys_refresh.go — BYPASSRLS scan, DOES depend on `service`
├── internal/
│   ├── core/
│   │   ├── domain/      # Entities, ErrorCode taxonomy (§17), event payloads
│   │   ├── port/        # Repositories, SecretStore, EventPublisher, RealmProvisionerClient, TxRunner, Inbox (inbox.go)
│   │   └── service/     # CredentialService (TS-1/TS-2), PrincipalService (TS-3..TS-6), JWKSService, secret_generator (RSA-2048)
│   └── adapter/
│       ├── inbound/
│       │   ├── http/      # router.go, middleware.go, errors.go, credential/principal/jwks handlers, /swagger, /asyncapi
│       │   └── consumer/  # offboarding_consumer.go + dedup.go (over port.Inbox)
│       └── outbound/
│           ├── postgres/  # Repositories incl. inbox_repository.go, rls_violation_repository.go, keys_refresh_repository.go,
│           │              #   jwks_tenants_repository.go; db.go, migrate.go, migrations/000001–000005
│           ├── openbao/   # KV v2 client, Kubernetes auth only
│           ├── eventbus/  # Outbox publisher + Glue / GlueDecoder / Noop / Validating codecs, embedded schemas
│           ├── realmprovisioner/  # RP-17 client
│           ├── httpx/     # Instrumented outbound http.RoundTripper
│           └── metrics/   # Tier 1/2/3 collectors, library init, OpenBao/RP/credential-service instrumentation
├── pkg/requestctx/      # Typed RequestContext{UserID, TenantID}
├── api/                 # asyncapi.yaml, embedded via //go:embed
├── deploy/
│   ├── helm/            # The chart (templates/validate.yaml holds the cross-template render guards)
│   ├── iam/             # IRSA policies: policy-server.json, policy-consumer.json, policy.tf.example
│   ├── openbao/         # policy.hcl, role.tf.example
│   └── monitoring/      # app-alerts.yml, slo-rules.yml, schema-registry-alerts.yml, prometheus-adapter-rule.yaml
├── docs/
│   ├── lld/             # The signed-off LLD (rev 1.4)
│   ├── observability/   # README, metric-registry.md (generated), runbooks.md, migration.md
│   ├── architecture/    # Mermaid sources
│   └── swagger/         # Generated by `make swag`
├── scripts/             # init-db.sql, init-floci.sh, merge_coverage.py
├── .github/             # workflows/ (9), scripts/ (gates, arch-lint, metricslint, release helpers), dependabot.yml
└── test/                # unit (incl. metricsstandard, consumer), contract, postgres (RLS, inbox, lock timeout,
                         #   keys_refresh, jwks tenants, hardening migration), integration (OpenBao, Glue), e2e, dbseed
```

### Dependency rules (`go-arch-lint`)

| Component | May import |
|---|---|
| `domain` | Nothing internal |
| `port` | `domain` |
| `service` | `domain`, `port`, `requestctx` |
| `postgres`, `openbao` (own components) | `domain`, `port` |
| `observability` (`outbound/metrics`) | `domain`, `port`, `service` |
| `adapters_outbound` (eventbus, realmprovisioner) | `domain`, `port`, `service`, `observability`, `httpx` |
| `adapters_inbound` (http, consumer) | `domain`, `port`, `service`, `requestctx`, `apispec`, `observability` |
| `reconciler_jobs` (`cmd/rotator`) | `domain`, `port`, `postgres`, `openbao`, `observability`, `adapters_outbound`; **not** `service` |
| `cmd` (server, consumer, scheduler) | Everything above, including `service` |

The rule worth remembering: **`cmd/rotator` may not depend on `service`**. It drives Postgres and OpenBao directly under a `BYPASSRLS` connection for cross-tenant enumeration (RLS-7), a privilege no HTTP or event path may hold. `cmd/scheduler` holds the same enumeration privilege but calls `CredentialService.IssueOrRotate`, so it sits under `cmd`.

### Shared libraries

`platform-pgcommon/v2` v2.0.0 (pool, RLS GUCs, per-transaction timeouts, `platform_db_*`), `platform-events/v2` v2.0.0 (envelope, outbox, inbox, SNS/SQS, queue-depth sampler), `platform-gincommon` v1.6.0 (middleware, logging, tracing, metrics registry, `/metrics`), `openbao/openbao/api/v2` v2.7.0.

---

## Data model

Two tenant-scoped tables under `FORCE ROW LEVEL SECURITY` (`service_account_principals`, `service_account_credentials`) and four RLS-exempt operational tables: `outbox_events` (owned by platform-events), `processed_events` (the consumer's inbox ledger), `rls_violation_log` (sampled RLS rejections) and `keys_refresh_pending` (owed RP-17 refreshes). Full detail: [`.claude/database-schema.md`](.claude/database-schema.md), LLD §4.

| Migration | Adds |
|---|---|
| `000001_schema` | Base schema: enums, `app_tenant_id()`/`rls_check_tenant()`, the two tables, `touch_row` triggers, RLS, `processed_events`, the `serviceaccount_app` / `_migrator` / `_reconciler` / `admin_readonly` roles |
| `000002_rotation_cadence` | `rotation_cadence_days`, `next_rotation_at`, `idx_sac_next_rotation` (§16 TSQ-6) |
| `000003_rls_violation_log` | `rls_violation_log`, the logging `rls_check_tenant()`, `prune_rls_violation_log()` (TS-D18) |
| `000004_hardening` | App grants on the outbox dead-letter table and ordering sequence; `app_tenant_id()` `search_path` pinned and revoked from `PUBLIC`; prune refuses a TTL under 7 days (TS-D21) |
| `000005_keys_refresh_pending` | `keys_refresh_pending(tenant_id PK, requested_at, intent_until NULL)`; app role CRUD, reconciler role `SELECT` (TS-D22/TS-D23) |

`keys_refresh_pending`: the sweep writes a marker in the same transaction as its revoke; the scheduler writes an **intent** (`intent_until` set) before `IssueOrRotate` and turns it into a committed marker (`intent_until` NULL) after the rotation and after any failed RP-17. Committed markers are retried at the start of every rotator and scheduler run; intents are ignored until they expire. A marker is cleared only up to the `requested_at` that was observed, so a newer request is never lost.

OpenBao path (frozen): `iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>`.

Nothing is deployed yet, so migrations are applied outright (no expand/contract). `MIGRATION_DATABASE_URL` connects directly, bypassing PgBouncer, because `pg_advisory_lock` is session-scoped.

---

## Events

- **Produced:** 5 frozen events on `iam-serviceaccount-events` (single producer, Glue registry `iam-serviceaccount-events`): `ServiceAccountRegistered`, `ServiceAccountCredentialIssued`, `…CredentialRotated`, `…CredentialRevoked`, `ServiceAccountRevoked`. They go through the transactional outbox, so a `2xx` guarantees the row exists. `GlueCodec` resolves each schema version **by definition** once at startup (`glue:GetSchemaByDefinition`); an unregistered definition fails startup. The Audit Log service is the one subscriber.
- **Consumed:** `TenantMembershipsPurged` on `tenant-lifecycle-tokensvc-q` (Glue-encoded by O&M, decoded by `GlueDecoder`). Pipeline: DLQ router → cascade metrics → consumed-schema validation → `Handle`. Dedup runs on platform-events' inbox (TS-D19). The whole cascade (inbox claim, OpenBao deletes, row deletes, `ServiceAccountRevoked` outbox rows) is one RLS-scoped transaction; any failure rolls the claim back and SQS redelivers.
- **Permanent rejects go straight to the DLQ** (`tenant-lifecycle-tokensvc-q-dlq`, URL from the queue's `RedrivePolicy`) with a `DLQReason` message attribute, then the source message is acked. Each one increments `iam_token_service_consumer_dlq_rejects_total{reason}`. If the DLQ cannot be resolved or the send fails, the message falls back to normal redrive.

| `DLQReason` | Cause | Notes |
|---|---|---|
| `schema_violation` | Payload fails the embedded `tenant_memberships_purged.json` | Also `iam_token_service_consumed_schema_violations_total` |
| `invalid_envelope_id` | A `TenantMembershipsPurged` with a missing or non-UUID envelope `id` | Cannot be deduplicated, and acking it would skip a GDPR erasure (TS-D22). Pages via `IAMTokenServiceOffboardingInvalidEnvelopeRejected` |

An unrecognised event type is **acked** (`unknown_event_acknowledged_total`), never retried, so a producer adding an event type cannot DLQ-storm this queue. `event_type` metric labels are a closed set; anything else is `other`.

Event contract: [`api/asyncapi.yaml`](api/asyncapi.yaml), LLD §7.

---

## Integrating with other services

| Direction | Service | Relationship |
|---|---|---|
| Called by | Realm Provisioner | TS-4 after it mints the client (RP-1/RP-2); TS-1 for provisioning. It discards the returned private key and calls RP-17 so Keycloak fetches the public key from this service's JWKS |
| Called by | Operators, O&M tooling | TS-1 (on-demand / break-glass rotation, then RP-17 by hand), TS-2 (revoke), TS-3 (read) |
| Called by | Org & Membership | TS-5 (find by sub, AUTH-9) |
| Called by | Workflow Service | TS-6 (the tenant's automation subject, TS-D17) |
| Called by | Keycloak | The EXT-6 JWKS route |
| Calls | Realm Provisioner | RP-17 from `cmd/rotator` (after automatic revokes) and `cmd/scheduler` (after cadence rotations). 2 attempts, retries 429/502/503/504, any 2xx is success, 3s per attempt |
| Consumes | Org & Membership / Core | `TenantMembershipsPurged` |
| Publishes to | Audit Log | 5 events on `iam-serviceaccount-events` |
| Never calls | Keycloak Admin API | TS-INV-1 |

Things to know as a caller:

1. **Register first.** TS-1 against an unregistered `principal_id` is `404 principal_not_found`.
2. **Retry TS-1 with the same `rotation_id`**, within the replay window, to recover a lost response without generating new material. Use a new `rotation_id` only for a genuinely new rotation.
3. **TS-1 and TS-2 alone change nothing at Keycloak** (TS-INV-7). An operator-driven rotate or revoke must be followed by RP-17. The scheduler and rotator do this themselves.
4. **Optimistic locking:** both tables carry `record_version`, bumped by the `touch_row()` trigger. A `409 optimistic_lock_conflict` returns `details.expected_version`. TS-2 and the sweep re-check after a conflict and report the idempotent outcome when two actors converged on the same result (TS-D13).
5. **Rate limits:** TS-1..TS-6 apply none (in-mesh callers only). Only the JWKS route is rate-limited (above).

---

## Local development

### Prerequisites

Go 1.26.9, Docker with Compose v2, and an SSH key registered with the BCBP-SOLUTIONS-FZC-LLC org (the Makefile sets `GOPRIVATE`). Building the **image** also needs a `.go_private_token` file (gitignored; a GitHub PAT with read access to the org's private repos). `make docker-up` does not need it.

### Quick start

```bash
make setup        # copies .env-example → .env, installs .githooks/pre-commit
make docker-up    # Postgres, PgBouncer, OpenBao (dev mode), Floci (SNS/SQS/Glue), Floci UI — infra only
make run          # cmd/server natively against that infra
make run-consumer # cmd/consumer (APP_PORT=8081, METRICS_PORT=9091)
```

OpenBao runs in dev mode and this service only supports Kubernetes auth, so a real credential round-trip fails locally until seeded by hand; `make test-integration` exercises the real login path against a fake TokenReview server.

### Make targets

| Command | Description |
|---|---|
| `make setup` / `make install-hooks` | `.env` from `.env-example`; install `.githooks/pre-commit` |
| `make tidy` / `make fmt` / `make fmt-check` / `make vet` | Go basics. `vet` runs twice: default build and with every test build tag |
| `make lint` | `golangci-lint` (`go tool`), default build + every test build tag |
| `make arch-lint` | `go-arch-lint` against `.go-arch-lint.yml` (same script CI runs) |
| `make gates` | Invariant gates: `no-gocloak` (TS-INV-1), `no-secret-log` (TS-INV-2), `set-local-only` (RLS-6), `gincommon-obs` (negative: rejects slog/stdlib-log/direct-zap/DefaultRegisterer/hand-rolled TracerProvider), `gincommon-wired` (positive: every binary calls all 8 required gincommon init functions), `pgcommon-obs` (negative: rejects raw pgxpool/pgx.Connect/database/sql), `pgcommon-wired` (positive: every binary wires ConfigFromEnv/NewPool/GUCSetFromContext), `platform-events-obs` (negative: rejects sqs.ReceiveMessage/sns.Publish), `platform-events-wired` (positive: server wires SNS publisher + outbox runner; consumer wires SQS consumer + inbox), `metrics-taxonomy` (Tier-1/2/3 prefixes, `_total`/`_seconds` suffixes) |
| `make metrics-lint` | platform-gincommon `metricslint` on a real `/metrics` scrape and the alert/rule/runbook references, plus the `metric-registry.md` drift check |
| `make metrics-inventory` | Regenerate `docs/observability/metric-registry.md` |
| `make vuln-check` | `govulncheck` v1.8.0 on `./...` (every package, `cmd/` included) |
| `make sast` | `gosec` (Go-code SAST), distinct from `vuln-check` (dependency CVEs) and Trivy (image CVEs) |
| `make mod-verify` | `go mod verify` |
| `make test` | Unit + contract + Postgres + integration, in parallel (Docker required) |
| `make test-ci` | Same with `-race` and merged coverage (what CI runs) |
| `make test-unit` / `test-contract` / `test-postgres` / `test-integration` / `test-e2e` | One suite each. Unit and contract need no Docker |
| `make race` | All four suites with `-race` |
| `make test-smoke` | CI image gate (`IMAGE_TAG`, `BINARY` required) |
| `make run` / `make run-consumer` / `make run-scheduler` | Run server / consumer / one scheduler pass (`METRICS_PORT=9092`) natively |
| `make build` | All four binaries to `bin/` |
| `make cover` / `make cover-func` | Coverage HTML / per-function summary |
| `make ci` | `tidy fmt-check vet lint arch-lint gates metrics-lint test-ci build` |
| `make docker-up` / `make docker-down` | Start / stop the infra containers |
| `make docker-run-app` / `docker-run-rotator` / `docker-run-scheduler` | Containerised server+consumer, or one rotator/scheduler run (needs `.go_private_token`) |
| `make swag` / `make swag-check` | Regenerate `docs/swagger/` / fail if it is stale |
| `make extract-schemas` / `schema-validate` / `schema-diff` | Derive and validate the 5 event schemas from `api/asyncapi.yaml` (no AWS) |
| `make schema-register` / `schema-verify` / `schema-prune` | Register, verify (`AVAILABLE` by definition, the same lookup the pod runs at startup) or list orphans in Glue. Need `GLUE_REGISTRY_SERVICEACCOUNT_NAME` and AWS credentials; `schema-prune EXECUTE=true` deletes |
| `make schema-pull` / `pin-base-images` / `godoc` / `docs-serve` / `clean` / `help` | Utilities |

### Calling the API locally

```bash
H=(-H "x-user-id: 00000000-0000-0000-0000-0000000000a1" -H "x-tenant-id: $TENANT_ID")

# TS-4 register
curl -s -X POST localhost:8080/api/v1/internal/tenants/$TENANT_ID/service-accounts "${H[@]}" \
  -H 'content-type: application/json' \
  -d '{"principal_sub":"<keycloak-sub-uuid>","keycloak_client_id":"platform-automation"}'

# TS-1 issue
curl -s -X POST localhost:8080/api/v1/internal/tenants/$TENANT_ID/service-accounts/$PRINCIPAL_ID/credentials "${H[@]}" \
  -H 'content-type: application/json' -d '{"rotation_id":"'"$(uuidgen)"'","overlap_seconds":300}'

# TS-3 read, and the public JWKS
curl -s localhost:8080/api/v1/internal/tenants/$TENANT_ID/service-accounts/$PRINCIPAL_ID "${H[@]}"
curl -s localhost:8080/api/v1/internal/tenants/$TENANT_ID/service-accounts/platform-automation/jwks.json
```

### Testing events locally

`make docker-up` runs `scripts/init-floci.sh`, which creates the SNS topic, `tenant-lifecycle-tokensvc-q` (+ DLQ), the `serviceaccount-audit-q` subscriber queue and the Glue registry with all 5 schemas, so the real Glue codec runs locally. Floci UI at **http://localhost:4502** shows queue message counts.

```bash
docker compose exec floci aws --region ap-south-1 sqs receive-message \
  --queue-url http://floci:4566/000000000000/serviceaccount-audit-q --max-number-of-messages 10
docker compose exec postgres psql -U serviceaccount_app -d serviceaccount -c \
  "SELECT event_type, published_at IS NOT NULL AS published, attempts FROM outbox_events ORDER BY created_at DESC LIMIT 20;"
```

### Local ports

| Service | In compose | From the host |
|---|---|---|
| API / `/metrics` | `server:8080` / `server:9090` | `localhost:8080` / `localhost:9090` |
| PostgreSQL (direct) | `postgres:5432` | `localhost:5535` |
| PgBouncer | `pgbouncer:5432` | `localhost:5536` |
| Floci (SNS/SQS/Glue) | `floci:4566` | `localhost:4568` |
| Floci UI | `floci-ui:4500` | `localhost:4502` |
| OpenBao | `openbao:8200` | `localhost:8210` |

---

## Testing

Canonical suites (do not break):

- `test/postgres` — real Postgres via testcontainers: RLS actually blocks cross-tenant access; the inbox cascade (redelivery no-op, failed cascade leaves the event unclaimed, concurrent deliveries run once); lock-timeout → `rotation_in_flight`; credential concurrency; `keys_refresh_pending`; the JWKS known-tenant query; the `000004` hardening migration; `rls_violation_log`.
- `test/integration` — real OpenBao with a fake Kubernetes TokenReview server (the Kubernetes-auth login end-to-end), and Floci Glue (versions resolved by definition).
- `test/unit/consumer`, `cmd/consumer` — ack vs retry vs DLQ semantics, the Glue-framed inbound pipeline and consumed-schema validation.
- `test/unit/metricsstandard` — registration order and the `domain`/`service`/`environment` identity on every series (`METRICS_SCRAPE_OUT` writes the scrape `make metrics-lint` uses).
- `test/e2e` — real router + real Postgres.

`make test-ci` scopes coverage to `internal/...` and `pkg/...` (`cmd/` main functions cannot be unit-invoked; e2e covers them) and CI enforces **≥ 98%**.

---

## Configuration

`.env-example` holds the dev values with comments. "Default" is what the code uses when the variable is unset (a library default where a shared library reads it). "Required" means startup fails outside `local`/`dev`/`test` when unset.

### Application and runtime

| Variable | Read by | Default | Notes |
|---|---|---|---|
| `APP_ENV` | all | `dev` | Also the metrics `environment` label: `local`/`dev`/`test`/`staging`/`prod` only (aliases such as `production` fail startup). Helm: `prod` |
| `APP_NAME` | all | `iam-token-service` | Metrics `service` label and trace service name (`OTEL_SERVICE_NAME` is not read) |
| `OBSERVABILITY_DOMAIN` | all | `iam` | Metrics `domain` label |
| `APP_PORT` | server, consumer | `8080` | API port (consumer: health router) |
| `METRICS_PORT` | all | `9090` | Dedicated `/metrics` listener |
| `SHUTDOWN_DRAIN_DELAY` | server, consumer | `5s` | After SIGTERM `/readyz` returns 503 for this long before listeners close |
| `LOG_LEVEL`, `LOG_SAMPLING` | gincommon | dev `debug`, `false` | Helm `info`, `false` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | gincommon | dev `none` | `none` disables export but keeps trace IDs. Helm renders `none` when `otel.exporterEndpoint` is empty |
| `OTEL_EXPORTER_OTLP_INSECURE`, `OTEL_TRACES_SAMPLER_RATIO`, `OTEL_PROPAGATOR_BAGGAGE`, `OTEL_TRACES_IGNORE_REMOTE_PARENT_SAMPLED` | gincommon | dev `true`, `1.0`, `false`, `false` | Helm `false`, `0.1`, `false`, `false` |
| `DOCS_ENABLED`, `DOCS_AUTH_TOKEN` | server | `false`, empty | `.env-example` sets `DOCS_ENABLED=true`. Outside local/dev/test, `DOCS_ENABLED=true` without a token fails startup |

### Database

| Variable | Read by | Default | Notes |
|---|---|---|---|
| `PG_HOST`/`PG_PORT`/`PG_USER`/`PG_PASSWORD`/`PG_DBNAME`/`PG_SSLMODE` or `DATABASE_URL` | pgcommon (app pool) | `localhost`/`5432`/—/—/—/`require` | `DATABASE_URL` wins. Dev: PgBouncer on `localhost:5536`, `sslmode=disable` |
| `PG_MAX_CONNS`/`PG_MIN_CONNS`/`PG_BOUNCER_MODE`/`PG_SLOW_QUERY_THRESHOLD` | pgcommon | `10`/`2` (`0` in bouncer mode)/`false`/`200ms` | Dev `20`/`0`/`true`/`200ms`; Helm `10`/`0`/`true`/`200ms` |
| `PG_STATEMENT_TIMEOUT`/`PG_LOCK_TIMEOUT` | pgcommon | unset (server setting) | Applied per transaction (`SET LOCAL`) on the app and reconciler pools, never on the migration DSN. Dev and Helm `5s`/`2s`; Helm `60s` statement timeout for the CronJobs. The lock timeout is what turns a contended principal lock into `409 rotation_in_flight` |
| `MIGRATION_DATABASE_URL` | all (migrate) | falls back to the app DSN | Direct connection as `serviceaccount_migrator` |
| `RUN_MIGRATIONS`, `MIGRATE_ONLY` | all / server | `true`, `false` | Helm sets `RUN_MIGRATIONS=false` on every workload and `MIGRATE_ONLY=true` on the migrate Job |
| `RECONCILER_DATABASE_URL` | server, rotator, scheduler | falls back to the app DSN | `serviceaccount_reconciler` (`BYPASSRLS`, `SELECT`-only). Without it cross-tenant reads return zero rows under RLS |

### OpenBao

| Variable | Default | Notes |
|---|---|---|
| `OPENBAO_ADDR` | dev `https://openbao.iam.svc.cluster.local:8200` | Required. `.env-example`: `http://localhost:8210` |
| `OPENBAO_ROLE`, `OPENBAO_KV_MOUNT` | `iam-token-service`, `iam` | Kubernetes-auth role, KV v2 mount |
| `OPENBAO_K8S_TOKEN_PATH` | empty = `/var/run/secrets/kubernetes.io/serviceaccount/token` | Helm points it at a projected token with audience `openbao.tokenAudience` (`openbao`) |
| `BAO_CACERT` | unset | Read by the OpenBao SDK. Helm sets it from `openbao.caCertSecret` |

### AWS, events and outbox

| Variable | Read by | Default | Notes |
|---|---|---|---|
| `AWS_REGION`, `AWS_ENDPOINT_URL` | server, consumer | `ap-south-1`, unset | Dev endpoint `http://localhost:4568` (Floci) with `test`/`test` keys. Deployed: IRSA, no keys |
| `GLUE_REGISTRY_NAME` | server | unset → `NoopCodec` (plain JSON) | Required outside dev/test (Helm `events.glueRegistryName`; validated by `deploy/helm/templates/validate.yaml`). All 5 produced schemas must be registered and `AVAILABLE` in the Glue registry **before** `helm upgrade` — the pod crash-loops on startup if any schema is missing (TS-RP-GAP-003). Run `make schema-register` then `make schema-verify` first |
| `SNS_TOPIC_ARN` | server | unset → no-op publisher | Required (`events.topicArn`) |
| `OUTBOX_POLL_INTERVAL`, `OUTBOX_PUBLISH_CONCURRENCY`, `OUTBOX_STARTUP_JITTER`, `OUTBOX_CLAIM_LEASE_DURATION` | server | `500ms`, `4`, `2s`, `10m` | Service defaults over the library's |
| `OUTBOX_BATCH_SIZE`, `OUTBOX_MAX_ATTEMPTS`, `OUTBOX_DRAIN_TIMEOUT`, `OUTBOX_PUBLISH_TIMEOUT` | server | `50`, `5`, `30s`, `10s` | platform-events defaults |
| `SQS_QUEUE_URL` | consumer | dev: local Floci queue | Required (`sqs.queueUrl`) |
| `SQS_CONCURRENCY`, `SQS_VISIBILITY_TIMEOUT`, `SQS_HANDLER_TIMEOUT`, `SQS_DRAIN_TIMEOUT`, `SQS_QUEUE_DEPTH_INTERVAL` | consumer | `2`, `60s`, `45s`, `15s`, `60s` | Handler timeout stays below the visibility timeout (the OpenBao deletes run inside the inbox transaction). `0s` disables depth sampling |
| `SQS_RETRY_BACKOFF`, `SQS_MAX_RETRY_BACKOFF` | consumer | `60s`, `15m` | Exponential redelivery backoff for a failed message (1m, 2m, 4m, 8m…), so `maxReceiveCount=5` covers ~15 minutes of outage before the DLQ. `0s` turns it off |
| `SQS_MAX_MESSAGES`, `SQS_WAIT_SECONDS` | consumer | `10`, `20` | platform-events defaults |

### Rotation, JWKS and jobs

| Variable | Read by | Default | Notes |
|---|---|---|---|
| `ROTATION_DEFAULT_OVERLAP_SECONDS` | server, scheduler | `300` | Startup fails outside `[0, 900]` (TS-CONFIG-4) |
| `ROTATION_DEFAULT_CADENCE_DAYS` | server, scheduler | `90` | Stamps `next_rotation_at` |
| `ROTATION_REPLAY_WINDOW` | server | `15m` | `0` = unlimited; negative or unparseable fails startup |
| `JWKS_RATE_LIMIT_RPS`, `JWKS_RATE_LIMIT_BURST` | server | `20`, `40` | Global bucket (known tenants) |
| `JWKS_RATE_LIMIT_PER_TENANT_RPS`, `JWKS_RATE_LIMIT_PER_TENANT_BURST` | server | `5`, `10` | Per-tenant bucket |
| `JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS`, `JWKS_RATE_LIMIT_UNKNOWN_TENANT_BURST` | server | `2`, `5` | Shared bucket for tenants with no live credential |
| `JWKS_KNOWN_TENANTS_REFRESH` | server | `15s` | Must be a positive duration or startup fails |
| `ROTATION_OVERLAP_GAUGE_INTERVAL`, `RLS_VIOLATION_EXPORTER_INTERVAL`, `KEYS_REFRESH_EXPORTER_INTERVAL` | server | `30s`, `1m`, `30s` | DB-state exporters over the reconciler pool |
| `REALM_PROVISIONER_BASE_URL` | rotator, scheduler | dev `http://iam-realm-provisioner.iam.svc.cluster.local:8080` | Required. `.env-example`: `http://localhost:8081` |
| `ROTATOR_RUN_TIMEOUT`, `SCHEDULER_RUN_TIMEOUT` | rotator, scheduler | `180s` | In-process run budget |
| `ROTATOR_METRICS_SCRAPE_GRACE`, `SCHEDULER_METRICS_SCRAPE_GRACE` | rotator, scheduler | `15s` | `/metrics` window before exit; budget + grace stay under `activeDeadlineSeconds` (240) |
| `ROTATOR_BATCH_LIMIT`, `SCHEDULER_BATCH_LIMIT` | rotator, scheduler | `500` | Rows per run; the rest wait for the next run |
| `PROCESSED_EVENTS_TTL_DAYS`, `RLS_VIOLATION_LOG_TTL_DAYS` | rotator | `8`, `30` | Prune retention (`rls_violation_log` refuses < 7) |
| `PRUNE_BATCH_LIMIT`, `OUTBOX_PRUNE_OLDER_THAN` | rotator | `10000`, `168h` | Prune batch size and published-outbox retention |

---

## Security

| Topic | Guidance |
|---|---|
| Tenant isolation | `FORCE ROW LEVEL SECURITY` on both tenant tables; no GUC means zero rows or a rejected write |
| GUC scoping | `SET LOCAL app.tenant_id` only (the `set-local-only` gate), never a session `SET` |
| Roles | `serviceaccount_app` (RLS-scoped), `serviceaccount_migrator` (DDL, migrate Job only), `serviceaccount_reconciler` (`BYPASSRLS`, `SELECT`-only; rotator, scheduler and the server's exporters; every write still goes through the app role) |
| RLS violations | `rls_check_tenant()` fails closed and samples rejections into `rls_violation_log`, exported as `iam_rls_violations_total` (TS-D18) |
| No Keycloak credential | No `gocloak`, no Admin credential (TS-INV-1, CI-enforced) |
| Secrets in Postgres or logs | Impossible by design (TS-INV-2); the `no-secret-log` gate also covers error strings and span attributes |
| **Istio `AuthorizationPolicy`** | **Required outside `local`/`dev`/`test`** (render fails). NetworkPolicy admits Keycloak and Workflow to the whole HTTP port, and TS-1..TS-6 authenticate only by the `x-user-id` header, which any pod can set. The policy limits `keycloakNamespaces` to `GET …/jwks.json`, `workflowNamespaces` to TS-6, and gives `meshNamespaces` every route. Keycloak and callers **must be in the mesh**: `source.namespaces` matches only mTLS peers |
| `PeerAuthentication` | `authorizationPolicy.strictMTLS` (default `true`) renders STRICT mTLS on the server; the metrics port stays PERMISSIVE for a non-mesh Prometheus |
| Sidecar injection | With the policy enabled, server and consumer pods are force-injected (the policy is enforced by the server's own sidecar), as **native sidecars** by default (`authorizationPolicy.nativeSidecar`, Istio ≥ 1.20 on Kubernetes ≥ 1.29) so the proxy outlives the app's drain. CronJobs are not injected unless `cronjobs.istioInject: true`; the migrate Job never is |
| NetworkPolicy | Ingress from `ingressNamespaceSelector` (mesh), `keycloakNamespaceSelector`, optional `workflowNamespaceSelector`; metrics port only from `monitoringNamespaceSelector`. Egress allow-list: DNS, Postgres (`postgresPort` and `postgresDirectPort`), OpenBao, AWS HTTPS, OTel, istiod (when injected), RP-17 (CronJobs only). Off-cluster targets by CIDR (`networkPolicy.egress.*CIDRs`) |
| AWS (IRSA) | One ServiceAccount per workload (`serviceAccount.perWorkload: true`). Only server (`deploy/iam/policy-server.json`: SNS publish, Glue read) and consumer (`policy-consumer.json`: SQS consume, DLQ send/depth) get roles; rotator, scheduler and migrate get none |
| OpenBao auth | Kubernetes auth only, projected token with audience `openbao`; the role binds the four workload ServiceAccounts (`deploy/openbao/role.tf.example`). ACL limited to `iam/{data,metadata}/serviceaccount/*`. Single-flight login, token refreshed `min(30s, lease/2)` before expiry, one re-login on a 403 |
| Break-glass revoke | TS-2 alone does not stop a key at Keycloak (TS-INV-7); follow with RP-17 |
| Docs | Bearer-locked outside dev; render and startup both fail if enabled without auth |
| SAST / CVEs | `gosec` (`make sast`), `govulncheck` (`make vuln-check`), Trivy on the image |

### Helm render guards

The chart refuses to render an unsafe install. Outside `local`/`dev`/`test` (`templates/validate.yaml`):

- `authorizationPolicy.enabled` must be `true`, with non-empty `meshNamespaces` and `keycloakNamespaces` (these two are required whenever the policy is enabled, in any environment);
- `events.topicArn` and `sqs.queueUrl` must be set;
- `docs.enabled` requires `docs.authEnabled`.

In any environment:

- `serviceAccount.perWorkload: false` with `workloadAnnotations` (other than the `migrate` key) fails: one shared ServiceAccount can carry only one IRSA role;
- with `networkPolicy.enabled` (`templates/networkpolicy.yaml`): `ingressNamespaceSelector`, `keycloakNamespaceSelector`, `monitoringNamespaceSelector` and `egress.postgresCIDRs` are required;
- without `existingSecret`/`database.existingSecretName`, every required `secretValues` key must be set.

---

## Observability

How this service implements the Enterprise Platform Observability Standard: [`docs/observability/`](docs/observability/README.md). Every metric, by tier: [`metric-registry.md`](docs/observability/metric-registry.md) (generated by `make metrics-inventory`). One runbook per alert: [`runbooks.md`](docs/observability/runbooks.md). Renamed/removed metrics: [`migration.md`](docs/observability/migration.md).

- **Identity:** every series carries `{domain="iam", service="iam-token-service", environment=APP_ENV}`, injected centrally; call sites never set them.
- **Tier 1** (`platform_*`, shared libraries): HTTP, messages/outbox/DLQ, `platform_queue_depth`/`platform_dlq_depth`, `platform_db_*` (`pool="default"` and `pool="reconciler"`), and `platform_dependency_request_seconds` for `dependency="openbao"` and `dependency="realm_provisioner",operation="refresh_keys"`.
- **Tier 2:** `iam_rls_violations_total{violation_type}` (every server replica exports the same rows: aggregate with `max`).
- **Tier 3** (`iam_token_service_*`): 17 business metrics, including `jwks_key_errors_total`, `jwks_rate_limited_total`, `jwks_rate_limited_by_bucket_total{bucket}`, `credential_replays_total{result}`, `keys_refresh_pending`, `keys_refresh_oldest_age_seconds`, `consumer_dlq_rejects_total{reason}`, `cadence_rotation_total`, `rotation_sweep_total`, `material_reconcile_total`.
- **Tracing/logs:** all four binaries call `gincommon.InitTracingWithConfig`; DB spans via `gincommon.NewSpanTracer`, job/consumer spans via `gincommon.NewTracer`; the span trace id is propagated onto produced event envelopes. JSON logs carry `trace_id` and never a credential field.

**Alerts** live in `deploy/monitoring/app-alerts.yml` (mirrored by the chart's `prometheusrule.yaml`) and `slo-rules.yml` (`prometheusrule-slo.yaml`). Ones worth knowing:

| Alert | Fires when |
|---|---|
| `IAMTokenServiceJWKSKeyErrors` | A live credential's key could not be served (page; may be a `503 jwks_keys_unavailable`) |
| `IAMTokenServiceJWKSRateLimited` | Known tenants got 429s on the JWKS route (unknown-bucket 429s excluded) |
| `IAMTokenServiceKeysRefreshBacklog` | The oldest owed RP-17 refresh is older than 15 minutes |
| `IAMTokenServiceCadenceRotationFailures` / `IAMTokenServiceRotationSweepFailures` | A scheduler / rotator run reported failures |
| `IAMTokenServiceSchedulerNotRunning` / `IAMTokenServiceRotatorNotRunning` / `IAMTokenServiceCronJobRunFailed` | A CronJob stopped running or its Job failed |
| `IAMTokenServiceOpenBaoErrors` / `IAMTokenServiceOpenBaoCallLatencyHigh` | OpenBao calls failing or slow |
| `IAMTokenServiceOffboardingInvalidEnvelopeRejected` | A `TenantMembershipsPurged` went to the DLQ as `invalid_envelope_id` (page: erasure pending) |
| `IAMTokenServiceConsumedSchemaViolation` / `IAMTokenServiceOffboardingDLQBacklog` | Schema-violation rejects / anything sitting in the DLQ |
| `IAMTokenServiceOrphanMaterialMissing` | A live credential has no OpenBao material (page) |
| `IAMTokenServiceRLSCrossTenantAccess` / `IAMTokenServiceRLSMissingGUC` | RLS rejections |

SLOs (LLD §11): SLO-1 TS-1 p99 ≤ 500 ms; SLO-2 99.9% of TS-1/TS-2/TS-4 non-5xx; SLO-3 99% of sweep runs without error (operational proxy); SLO-4 TS-3 p99 ≤ 100 ms.

---

## Deployment

### Helm chart

`deploy/helm/` renders: server and consumer Deployments (`replicaCount: 2` each, for HA), rotator and scheduler CronJobs (`*/5`, `concurrencyPolicy: Forbid`, `activeDeadlineSeconds: 240`, exit 1 fails the Job at once), the migrate hook Job, per-workload ServiceAccounts, NetworkPolicy, Istio `AuthorizationPolicy` + `PeerAuthentication`, PodDisruptionBudget (`minAvailable: 1`), ServiceMonitor + PodMonitor (CronJob pods, scraped every 5s) and the two PrometheusRules. HPA and Ingress/HTTPRoute/SecurityPolicy ship disabled (no public surface).

**Hooks.** With `migrations.enabled` (default), the migrate Job runs pre-install/pre-upgrade at weight 0. Its ServiceAccount, the chart Secret (`secretValues`) and the migrate NetworkPolicy are hooks at weight -10, so they exist before the Job on a first install. Hook resources are outside the release manifest: `helm rollback` does not revert the hook Secret (see the `secretValues` comment in `values.yaml`).

**Grace periods.** Server `terminationGracePeriodSeconds: 80` (drain delay + HTTP drain + outbox drain), consumer `60`, CronJobs `45` (finish the current tenant). Readiness probe `timeoutSeconds: 3`. Pod anti-affinity is required across nodes; the server's becomes preferred when the HPA is on, since it may scale past the node count.

**Values that matter** (read `values.yaml` for the rest; every variable the chart sets is read by a binary or shared library, TS-D20):

| Value | Purpose |
|---|---|
| `appEnv` | `prod` by default; anything outside `local`/`dev`/`test` turns on the render guards |
| `image.digest` | Pins every workload to the scanned, signed digest (wins over `tag`) |
| `serviceAccount.perWorkload`, `serviceAccount.workloadAnnotations` | Per-workload IRSA roles (`server`, `consumer`) |
| `authorizationPolicy.{enabled,meshNamespaces,keycloakNamespaces,workflowNamespaces,strictMTLS,nativeSidecar}` | L7 policy, STRICT mTLS, native sidecars |
| `networkPolicy.{ingress,keycloak,workflow,monitoring}NamespaceSelector`, `networkPolicy.egress.*CIDRs`, `postgresPort`/`postgresDirectPort` | L4 policy |
| `cronjobs.istioInject` | Inject the CronJobs (native sidecar) when RP-17 requires mTLS |
| `database.existingSecretName` / `existingSecret` | `DATABASE_URL`, `MIGRATION_DATABASE_URL`, `RECONCILER_DATABASE_URL` (never in values) |
| `database.pool.*` | Pool size and timeouts; `jobStatementTimeout: 60s` for the CronJobs |
| `openbao.{addr,authRole,kvMount,tokenAudience,caCertSecret}` | OpenBao login |
| `events.{topicArn,glueRegistryName}`, `sqs.*` | Event plumbing |
| `realmProvisioner.baseUrl` | RP-17 target |
| `rotation.{defaultOverlapSeconds,defaultCadenceDays,replayWindow}` | Rotation behaviour |
| `jwks.*` | JWKS rate limits and `knownTenantsRefresh` |
| `exporters.{rotationOverlapInterval,rlsViolationInterval,keysRefreshInterval}` | Server exporter intervals |
| `rotator.*`, `scheduler.*` | Schedule, `runTimeout`, `metricsScrapeGrace`, `batchLimit` (rotator also `pruneBatchLimit`, `outboxPruneOlderThan`) |
| `shutdown.drainDelay`, `migrations.*`, `docs.*`, `otel.*`, `serviceMonitor.*` | As named |

### Before you deploy

1. Postgres roles exist (`scripts/init-db.sql` shows them) and a Secret provides the three DSNs (`database.existingSecretName`).
2. The SNS topic, `tenant-lifecycle-tokensvc-q` (with a `RedrivePolicy` DLQ) and the Glue registry exist; set `events.topicArn` and `sqs.queueUrl`.
3. All 5 schema definitions are registered and `AVAILABLE` (`make schema-verify`); otherwise the server refuses to start. The release pipeline registers them before deploying.
4. IRSA roles for server and consumer (`deploy/iam/`), wired through `serviceAccount.workloadAnnotations`.
5. The OpenBao role (`deploy/openbao/role.tf.example`) binds the four workload ServiceAccounts with audience `openbao`; the policy is `deploy/openbao/policy.hcl`.
6. Keycloak, the Realm Provisioner, O&M and Workflow are in the mesh; set the `authorizationPolicy` namespaces and the matching NetworkPolicy selectors.
7. NetworkPolicy egress CIDRs for Postgres (required) and, if off-cluster, OpenBao, OTel and DNS.
8. If the Realm Provisioner enforces STRICT mTLS, set `cronjobs.istioInject: true`, or every RP-17 call fails and refreshes pile up in `keys_refresh_pending`.

---

## CI and release

Nine workflows; every action is pinned by commit SHA, and Dependabot (`.github/dependabot.yml`) is enabled. Required secret: `GO_PRIVATE_TOKEN`.

- **`ci.yml`** (push to `main`, PRs; skipped for Markdown-only commits): `validate-test` and `validate-quality` in parallel with `build-image` (Hadolint, cached build) → Trivy scan and smoke tests of all four binaries → PR summary. On push to `main`: build and push a **candidate** tag → scan the pushed digest → smoke-test that digest → Cosign sign and verify → only then point `main` / `sha-<short>` at it.
- **`validate-test.yml`**: `make test-ci` → coverage gate (≥ 98%) → arch-lint → Swagger staleness → event-schema sync → `make test-e2e`.
- **`validate-quality.yml`**: mod verify, fmt, tidy, `make vet`, `make lint`, `make swag-check`, `make gates`, `make metrics-lint`, `make vuln-check`, `make sast`, Dockerfile base-image digest check.
- **`release.yml`** (`v*` tag): preflight (must run from a tag ref) → validate → build (tag/CHANGELOG check, cross-compiled binaries) → docker (candidate push, Trivy, SBOM, provenance, smoke, Cosign sign, then publish the release tags) → `register-schemas` (calls `schema-registry.yml` for the production Glue registry) → `deploy-gate` (only when `vars.DEPLOY_GATE_ENABLED`; deploys by digest with `HELM_VALUES_B64` and `KUBECONFIG_B64`) → GitHub Release. Pre-release tags (`-rc` etc.) skip `register-schemas` and `deploy-gate`.
- **`schema-registry.yml`**, **`schema-prune.yml`**, **`schema-health-quarterly.yml`**, **`freeze-watchdog.yml`**: Glue schema governance. **`changelog-check.yml`**: a `CHANGELOG.md` entry on runtime-affecting PRs.

No `v*` tag exists yet; pin `main` images by digest.

---

## Troubleshooting

| Symptom | Likely cause | What to do |
|---|---|---|
| JWKS route `503 jwks_keys_unavailable` | The tenant's active key (or every key) could not be read from OpenBao: OpenBao down, login failing, or material missing | Keycloak keeps its cached keys meanwhile. Check OpenBao health and the server log `jwks: skipping unreadable credential`; if material is gone, rotate (TS-1) then RP-17. Runbook: `IAMTokenServiceJWKSKeyErrors` |
| JWKS route `429 rate_limited` | Per-tenant, global or unknown-tenant bucket exhausted. A brand-new tenant counts as unknown for up to `JWKS_KNOWN_TENANTS_REFRESH` (15s) | Check `iam_token_service_jwks_rate_limited_by_bucket_total{bucket}`: `unknown` = a scan of random ids (block at ingress); `tenant`/`global` = real traffic, raise `jwks.*`. Unexpected callers mean the `AuthorizationPolicy` is too wide |
| `409 rotation_in_flight` | Another TS-1/TS-2/offboarding held the principal lock longer than `PG_LOCK_TIMEOUT` (2s) | Retry. To repeat the same rotation, reuse the same `rotation_id`; `details.active_rotation_id`, when present, names the competing one |
| `409 credential_replay_expired` | A `rotation_id` replayed more than `ROTATION_REPLAY_WINDOW` (15m) after issue | Not a fault: start a new rotation with a fresh `rotation_id`. `409 credential_replay_revoked` means the version is gone |
| `keys_refresh_pending` rows not draining / `IAMTokenServiceKeysRefreshBacklog` | RP-17 keeps failing, or the CronJobs are not running | `SELECT tenant_id, requested_at, intent_until FROM keys_refresh_pending ORDER BY requested_at;`. Check Realm Provisioner health, the CronJob alerts, egress `realmProvisionerPort` and `cronjobs.istioInject`. Rows with `intent_until` in the future are a scheduler run in progress, not a backlog |
| `TenantMembershipsPurged` in the DLQ with `DLQReason=invalid_envelope_id` | Producer sent a missing or non-UUID envelope `id` | That tenant's credentials are **not** revoked yet. Give the message a valid UUID `id` and redrive; never discard it. Producer must fix its ids |
| DLQ message with `DLQReason=schema_violation` | Payload failed `tenant_memberships_purged.json` | Fix the producer (or `api/asyncapi.yaml` + `make extract-schemas` if ours is wrong), then redrive |
| `helm install/upgrade` fails: `authorizationPolicy.enabled must be true …` | Non-dev `appEnv` without the Istio policy | Enable it and set `meshNamespaces`/`keycloakNamespaces` (Keycloak must be in the mesh) |
| Render fails: `events.topicArn is required` / `sqs.queueUrl is required` | Non-dev `appEnv` without the event plumbing | Set the values |
| Render fails: `docs.enabled requires docs.authEnabled` | Docs on without auth outside dev | Set `docs.authEnabled: true` and `DOCS_AUTH_TOKEN` in the Secret, or disable docs |
| Render fails: `serviceAccount.workloadAnnotations is set but serviceAccount.perWorkload is false` | Per-workload IRSA on a shared ServiceAccount | Use `perWorkload: true`, or put the union role in `serviceAccount.annotations` |
| Render fails on a `networkPolicy.*Selector` or `postgresCIDRs` | Required NetworkPolicy values left empty | Set real selectors and the RDS CIDRs |
| Server panics `resolve glue schema … by definition` | This build's schema is not registered (`AVAILABLE`) in Glue | `make schema-verify`; register via `schema-registry.yml` or `make schema-register`. Locally, recreate Floci or unset `GLUE_REGISTRY_NAME` |
| Startup panic `required env var … is not set` | A required variable is missing outside local/dev/test | The message names the Helm value to set |
| `outbox_events` rows never get `published_at` | `SNS_TOPIC_ARN` mismatch or the outbox runner is not ready (`/readyz` shows `outbox: down`) | Compare `.env` with `aws sns list-topics` (or Floci UI) |
| Floci never reports healthy | Its healthcheck waits for the last Glue schema `init-floci.sh` creates | Check `docker compose logs floci`; the first 10–20s are normal |

---

## Out of scope

- Human-user tokens, JWT issuance or any login path.
- Authorization decisions or role/membership grants for the automation principal.
- Configuring the Keycloak client itself (Realm Provisioner only).
- Tenant-owned bots or user PAT self-service (documented Phase-2 extension).
- A tenant-facing API. The only route reachable without the system principal is the JWKS route, and it is restricted to Keycloak by the mesh.

---

## Contributing

Open a PR against `main`. CI must pass (`make ci` rehearses it locally) and `CHANGELOG.md` needs an entry for any runtime-behaviour change.

| Document | Description |
|---|---|
| [`docs/lld/iam-lld-token-service.md`](docs/lld/iam-lld-token-service.md) | The signed-off LLD: §16 open questions, §17 error taxonomy, §22 decision register, §24 runbooks, §25 frozen names |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | Architecture narrative with diagrams |
| [`docs/observability/`](docs/observability/README.md) | Observability Standard implementation, metric registry, alert runbooks |
| [`docs/observability-registry-proposals.md`](docs/observability-registry-proposals.md) | Tier 1/Tier 2 metric registry submissions |
| [`deploy/iam/README.md`](deploy/iam/README.md) | IRSA roles and grants |
| [`.claude/CLAUDE.md`](.claude/CLAUDE.md) and siblings | Quick-reference context: architecture, schema, API/events, request flows, operations |

---

## License / ownership

Internal service, owned by the BCBP Solutions IAM/Platform team (`platform@bcbpsolutions.com`). Not for external distribution.
