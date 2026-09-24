# iam-token-service

**Custodian of the platform-automation service account's rotating credential material** for the Tender Management SaaS Platform's IAM subsystem — issues, rotates, and revokes the per-tenant `platform-automation` Keycloak client's secret, custodies the plaintext exclusively in OpenBao (never Postgres), and drives the tenant-offboarding credential cleanup cascade.

**Repository:** `github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service`
**Module:** Go 1.26.6 — four binaries (`cmd/server`, `cmd/consumer`, `cmd/rotator`, `cmd/scheduler`) built from one image, deployed as a Helm chart (Deployment × 2, CronJob × 2)
**Design:** `docs/iam-lld-token-service.md` (rev 1.3, Approved) — this README and `ARCHITECTURE.md` are navigable summaries of it, not a replacement; the LLD is the tie-breaker on any discrepancy.

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

**One write-ordering discipline, used everywhere in this service (no
per-flow variation the way the Realm Provisioner has two):**
1. **Material-first, then Postgres commit, then RP-17 after the commit.**
   Every write (TS-1 issue/rotate, TS-2 revoke, the overlap sweep, the
   offboarding cascade) writes or deletes the OpenBao material *before*
   any Postgres transaction opens. If that step fails, nothing is
   committed and the caller gets `502 secret_store_unavailable` — this
   service has no "record the intent, reconcile later" discipline the way
   the Realm Provisioner's RP-8 does; every write here is fail-closed by
   design, because a service-account principal has no operator watching a
   `pending` queue the way a tenant admin does. Only once the OpenBao step
   succeeds does `RunInTx` commit the row and enqueue the outbox event
   atomically. `ClearServiceAccountKeysCache` (RP-17) then runs strictly
   *after* that commit, never before and never inside the same
   transaction — it is the one step this service cannot roll back if it
   fails, so ordering it last means a failure there never leaves an
   inconsistent local row, only a documented, page-worthy two-halves gap
   (TS-INV-7, TS-D14/TS-D15). See
   [Write ordering discipline](ARCHITECTURE.md#write-ordering-discipline)
   for the full sequence diagram.

---

## Why this service exists

The automation principal's credential needs a home that is neither
Keycloak (which only the Realm Provisioner may write) nor a general secrets
service with no domain awareness of rotation-overlap, idempotent
issue/rotate semantics, or the offboarding cascade. Concretely:

- **One holder of the credential-lifecycle logic.** Issue/rotate/revoke
  idempotency, overlap-window math, and the offboarding cascade are
  implemented once, in one codebase, instead of re-derived (and
  potentially re-broken) by every caller that would otherwise talk to
  OpenBao directly.
- **A structural, not just conventional, split from Keycloak
  administration.** If this service could also write to Keycloak, the
  two-halves model (system-of-record vs. applier) would collapse into a
  single point that both custodies material *and* can apply it — a wider
  blast radius for exactly the same compromise. TS-INV-1 keeps that
  impossible: this service has **zero** `gocloak` dependency, enforced
  structurally by `.github/scripts/check-no-gocloak.sh` (`make gates`) —
  a `Nerzal/gocloak` import anywhere in this repo fails the build, not
  just a review comment.
- **Plaintext custody with one exit door.** TS-INV-2 (no secret plaintext
  in Postgres, ever) and the CI `no-secret-log` gate (fails the build if a
  credential field name reaches a log/trace sink) together mean the
  *only* place a generated private key exists outside a single HTTP
  response body is OpenBao.
- **A stable, versioned contract for every caller.** The Realm Provisioner,
  Org & Membership, and operator tooling integrate against this service's
  HTTP/event contract instead of OpenBao's own KV API and its
  version-specific quirks, or Keycloak's client-jwt configuration
  directly.

---

## API overview

7 routes (TS-1..TS-6 plus the EXT-6 JWKS route) plus health/docs, all
registered from `internal/adapter/inbound/http/router.go`'s `NewRouter` —
the single source of truth; the generated Swagger spec (`make swag`,
`docs/swagger/`) is derived from `swaggo` annotations on the handlers,
never hand-authored.

**Middleware chain** (`NewRouter` → `registerInternalRoutes`): a 1 MB
`http.MaxBytesReader` body cap (this service's bodies are UUIDs and one
secret string — no legitimate caller needs more) → `gincommon.TimeoutMiddleware(30s)`
→ `gincommon.ObservabilityMiddlewares` → **then**, only on the
`/api/v1/internal` route group, `gincommon.ProtectedMiddlewares`
(`RequireAuth` + `ContextMiddleware`) → `GUCBridgeMiddleware` (binds
`app.tenant_id` transaction-locally, RLS) → `RequireJSONContentType` →
`RequireTenantPathMatch` (the `:id` path segment must equal the resolved
tenant GUC before any handler runs — a mismatch is `403`, never silently
corrected). `/healthz`, `/readyz`, the docs routes, and the JWKS route are
all registered *before* that protected group, so they never pick up
`RequireAuth`/`GUCBridgeMiddleware` at all — see the JWKS row below.

**Idempotency model — deliberately different from a header-based scheme.**
This service has no `Idempotency-Key` header mechanism: TS-1 takes a
**body field**, `rotation_id`, because the idempotency key here is
intrinsically part of the request's meaning (which rotation this is), not
an opaque retry token layered on top. Reusing the same `rotation_id`
against an in-flight or already-committed rotation replays the stored
result (`200`, not `201`); a *different* `rotation_id` arriving while one
is still in flight is `409 rotation_in_flight`. TS-2/TS-3/TS-4/TS-5/TS-6 are
naturally idempotent from their own request shape (a revoke of an
already-revoked version, or a repeat registration, is a no-op success)
and need no separate token at all.

### Internal routes (6)

| # | Method & path | Idempotency | Purpose | Emits |
|---|---|---|---|---|
| TS-4 | `POST /api/v1/internal/tenants/:id/service-accounts` | On `(tenant_id, keycloak_client_id)` — repeat returns `200` with the existing row, never a duplicate | Register the principal after the Realm Provisioner mints the Keycloak client | `ServiceAccountRegistered` |
| TS-1 | `POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials` | Body field `rotation_id` — same id replays (`200`); a different concurrent id is `409 rotation_in_flight` | Issue or rotate the credential — the only endpoint that ever returns the plaintext private key, exactly once | `…CredentialIssued` / `…CredentialRotated` |
| TS-2 | `POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials/:version/revoke` | Revoking an already-revoked version is a no-op success | Revoke one credential version immediately; deletes its OpenBao material | `…CredentialRevoked` |
| TS-3 | `GET /api/v1/internal/tenants/:id/service-accounts/:principal_id` | Read-only | Read principal + credential **metadata** — never a key | — |
| TS-5 | `GET /api/v1/internal/tenants/:id/service-accounts?principal_sub=<uuid>` | Read-only | Find a principal by its Keycloak `sub` instead of this service's own internal id (AUTH-9) — identity/status only, never credential metadata; used by org-membership's non-member defense-in-depth check | — |
| TS-6 | `GET /api/v1/internal/tenants/:id/service-accounts/platform-automation` | Read-only | Read the tenant's automation principal, including its Keycloak `sub` (TS-D17). It's the reverse of TS-5, and the Workflow Service's connector workers use it to name the acting principal. The sub is stable across rotation and changes on an RP-3/RP-4 re-mint; `principal_id` is the stable handle | — |
| — | `GET .../service-accounts/platform-automation/jwks.json` | Read-only, unauthenticated | EXT-6: public JWK Set for the tenant's platform-automation principal — the keys Keycloak's client-jwt authenticator fetches | — |

There are no "operator-only" routes on this service the way the Realm
Provisioner has RP-13/RP-14 behind `RequireOperatorRole` — every TS-1..TS-6
route accepts the same one caller identity, the reserved system principal
(`x-user-id: 00000000-0000-0000-0000-0000000000a1`); there is no
`platform_operator`-vs-`iam-system` distinction anywhere in this service's
authorization surface, because a human operator calling TS-1/TS-2 goes
through the same mesh path as any other caller, not a separately-gated one.

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

Like the Realm Provisioner, this service is **fail-fast, single-error**:
`ShouldBindJSON` stops at the first malformed field and a bind failure is
wrapped into one `400 invalid_request` — never a field-level array. A
handler-level check (a path-param UUID, a body-field UUID string) fails
before the request ever reaches the service layer; a business-rule check
(the clamp on `overlap_seconds`, the frozen `keycloak_client_id` value)
lives in the service layer instead, so it can never be bypassed by a
caller that skips the HTTP layer (a future gRPC/internal-only caller,
say). Either way, the response shape is the same flat envelope:

```json
{
  "error": "invalid_request",
  "status": 400,
  "trace_id": "a3f1b2c8d4e5f60718293a4b5c6d7e8f",
  "request_id": "01H8XYZ...",
  "details": { "field": "rotation_id" }
}
```

### Notable validation rules

| Field | Rule (as implemented) | Error / code |
|---|---|---|
| `id` (tenant, path) | Must parse as `uuid.UUID`; must equal the `x-tenant-id`-derived GUC before any DB checkout (`RequireTenantPathMatch`) | `400 invalid_request` (malformed) / `403` (mismatch — never silently corrected) |
| `principal_id` (path, TS-1/TS-2/TS-3) | Must parse as `uuid.UUID` | `400 invalid_request` |
| `rotation_id` (TS-1 body) | Empty is passed through as `uuid.Nil` so the service layer's own "required" check fires with the same shape; present-but-unparseable is rejected at the HTTP boundary | `400 invalid_request` |
| `overlap_seconds` (TS-1 body) | `nil` (omitted) means "use the configured default" (`ROTATION_DEFAULT_OVERLAP_SECONDS`, 300s); an explicit `0` is a deliberate hard cutover; either way `domain.ClampOverlapSeconds` bounds the effective value to `[0, 900]` regardless of what the caller sends | Never rejected — silently clamped |
| `version` (path, TS-2) | Must parse as an integer | `400 invalid_request` |
| `principal_sub` (TS-4 body) | Must parse as `uuid.UUID` at the HTTP layer (the service's `RegisterRequest.PrincipalSub` is already typed `uuid.UUID`, so a non-UUID string can never reach it) | `400 invalid_request` |
| `keycloak_client_id` (TS-4 body) | Must equal the frozen `platform-automation` value — a value-level business rule, checked in the service layer, not the handler | `400 invalid_request` |
| `principal_sub` (TS-5 query param) | Must parse as `uuid.UUID` | `400 invalid_request` |
| `x-user-id` (header, every route) | Must be the reserved system principal `00000000-0000-0000-0000-0000000000a1` — a tenant-facing principal is rejected before any handler runs | `401 missing_identity_headers` |
| `Content-Type` (header) | Must be `application/json` on a non-empty write body (`RequireJSONContentType`) | `415 unsupported_media_type` |

Mutation responses always include `record_version` so callers can
round-trip the optimistic-concurrency token (§9 below); no response body
ever includes a stored secret except TS-1's freshly-generated plaintext,
returned exactly once. Validation-relevant fields are never logged with
their raw values — only `tenant_id`, `principal_id`, `version`, `op`,
`result` (§ Observability below), enforced structurally by the CI
`no-secret-log` gate.

---

## Architecture

Clean Architecture — dependencies point inward; outer layers never import
inner layers: `domain` ← `port` ← `service` ← `adapter` ← `cmd`,
structurally enforced by `.go-arch-lint.yml` (`make lint` fails the build
on a layering violation, not just a style warning). Four composition
roots ship from one Docker image. Full diagrams and mechanism-by-mechanism
detail live in [`ARCHITECTURE.md`](ARCHITECTURE.md); this is the summary.

```
iam-token-service/
├── cmd/
│   ├── server/                        # HTTP composition root: pool+GUC wiring, migrations, outbox runner
│   ├── consumer/                      # SQS composition root: the one inbound subscription (offboarding) — GlueDecoder, DLQ router, consumed-schema validation
│   ├── rotator/                       # CronJob binary: overlap sweep + orphan reconciler + prune — BYPASSRLS, no `service` dependency (RLS-7)
│   └── scheduler/                     # CronJob binary: cadence-driven auto-rotation (§16 TSQ-6, TS-D14) — BYPASSRLS scan, but DOES depend on `service`
├── internal/
│   ├── core/
│   │   ├── domain/                    # Entities, value objects, ErrorCode taxonomy (§17), event payload structs
│   │   ├── port/                      # CredentialRepository, PrincipalRepository, SecretStore, EventPublisher, RealmProvisionerClient, TxRunner, ReconcilerRepository
│   │   └── service/                   # CredentialService (TS-1/TS-2), PrincipalService (TS-3/TS-4/TS-5/TS-6), JWKSService, secret_generator (RSA-2048 keypairs)
│   └── adapter/
│       ├── inbound/
│       │   ├── http/                  # Gin handlers (TS-1..TS-6), JWKS handler, middleware, router.go, /swagger, /asyncapi
│       │   └── consumer/              # The one SQS consumer: offboarding_consumer.go + dedup.go (ackUnknown semantics)
│       └── outbound/
│           ├── postgres/              # Repository impls, RLS/GUC wiring, migrations — its own component (not folded into adapters_outbound)
│           ├── openbao/               # KV v2 client, Kubernetes-auth only — its own component, mirroring TS-INV-1's isolation shape
│           ├── eventbus/              # Outbox publisher (topic iam-serviceaccount-events) + Glue (version by definition)/GlueDecoder/Noop/Validating codecs
│           ├── realmprovisioner/      # RP-17 (ClearServiceAccountKeysCache) HTTP client — the one outbound dependency on another IAM service
│           ├── httpx/                 # Shared instrumented http.RoundTripper (traceparent injection) that realmprovisioner builds on
│           └── metrics/               # iam_token_service_* (Tier 3) + platform_*/iam_* (Tier 1/2 proposed) Prometheus counters/histograms
├── pkg/requestctx/                    # Typed RequestContext{UserID, TenantID}
├── api/                                # //go:embed asyncapi.yaml — served from the compiled binary, no source tree in the image
├── docs/swagger/                      # swag-generated OpenAPI spec — checked in, regenerated by `make swag`
├── deploy/                            # Helm chart, OpenBao policy, Prometheus alerts
├── docs/iam-lld-token-service.md      # Frozen LLD, rev 1.3 Approved
├── .githooks/pre-commit               # tidy + fmt-check + vet + lint; installed via `make setup`/`make install-hooks`
└── test/                              # test/unit (black-box + white-box), test/contract, test/postgres (RLS, -tags=integration), test/integration, test/e2e, test/dbseed
```

### Dependency rules (enforced by `go-arch-lint` in CI)

| Package | May import |
|---|---|
| `core/domain` | Nothing internal (`anyVendorDeps: true` for `google/uuid` etc.) |
| `core/port` | `core/domain` only |
| `core/service` | `core/domain`, `core/port`, `pkg/requestctx` |
| `postgres` (own component) | `core/domain`, `core/port` |
| `openbao` (own component) | `core/domain`, `core/port` |
| `httpx` (own component) | — (shared outbound-HTTP transport `adapters_outbound` builds on) |
| `observability` (own component) | `core/domain`, `core/port`, `core/service` — a documented cross-cutting leaf |
| `adapters_outbound` (eventbus, realmprovisioner) | `core/domain`, `core/port`, `core/service`, `observability`, `httpx` |
| `adapters_inbound` (http, consumer) | `core/domain`, `core/port`, `core/service`, `requestctx`, `apispec`, `observability` |
| `reconciler_jobs` (`cmd/rotator` only) | `core/domain`, `core/port`, `postgres`, `openbao`, `observability`, `adapters_outbound` — **not** `core/service` |
| `cmd` (server/consumer/scheduler) | Everything above, including `service` — `cmd/scheduler` lives here, not in `reconciler_jobs`, precisely because it needs `service` |

The one rule most worth internalizing: **`cmd/rotator` may not depend on
`service`** — it drives `postgres`/`openbao` directly under a `BYPASSRLS`
connection for cross-tenant enumeration (RLS-7), a privilege no HTTP- or
event-triggered code path may ever hold. `cmd/scheduler` (§16 TSQ-6
Resolved) holds that same `BYPASSRLS` enumeration privilege for its own
due-list scan, but — unlike `cmd/rotator` — is *not* walled off from
`service`: it calls `CredentialService.IssueOrRotate` directly, so it is
listed under `cmd`'s own dependency allowance in `.go-arch-lint.yml`, not
`reconciler_jobs`'s.

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

Operators and O&M tooling call this to generate a fresh RSA-2048 keypair
and obtain the plaintext private key once, then call the Realm
Provisioner's RP-17 endpoint (`ClearServiceAccountKeysCache`) so Keycloak
re-fetches this service's JWKS and recognizes the new key — no key
material is ever sent to RP (EXT-6/rev 1.3); Keycloak fetches the public
key itself. This is a manually-orchestrated two-call sequence, not an
automated call from this service to RP. Automatic cadence-driven rotation
also exists (LLD §16 TSQ-6 — Resolved rev 1.2, TS-D14): a fourth binary,
`cmd/scheduler`, scans for principals past their `next_rotation_at` and
performs the same TS-1-then-RP-17 sequence itself. This service's own
`cmd/rotator` CronJob sweeps expired overlaps and reconciles orphaned
material (§8.3/§8.6); it never initiates a rotation, but it does call
RP-17 after every automatic revoke, for the same reason (TS-D15) —
Keycloak caches whatever key it last fetched with no self-expiring TTL, so
skipping that call would leave a revoked key still authenticating
indefinitely. The caller
supplies a `rotation_id` — reuse
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

Every non-2xx response is the flat `platform-gincommon` `ErrorResponse`
shape (`internal/adapter/inbound/http/errors.go`) — `{error, status,
trace_id, request_id, details?}`. There is no separate field-level
validation-error shape; a bind failure is `400 invalid_request` with the
same flat envelope.

```go
var errResp struct {
    Error     string         `json:"error"`
    Status    int            `json:"status"`
    TraceID   string         `json:"trace_id"`
    RequestID string         `json:"request_id"`
    Details   map[string]any `json:"details"`
}
_ = json.NewDecoder(resp.Body).Decode(&errResp)
// errResp.TraceID links to the distributed trace
```

| Code | Status | Trigger |
|---|---|---|
| `invalid_request` | 400 | Malformed JSON / a UUID-parse or business-rule field failure; `details.field` names the offending field |
| `missing_identity_headers` | 401 | `x-user-id`/`x-tenant-id` missing, or `x-user-id` is not the reserved system principal |
| `principal_not_found` | 404 | TS-1/TS-2/TS-3 against an unregistered `principal_id` |
| `rotation_in_flight` | 409 | TS-1 — a *different* `rotation_id` arrives while another rotation for the same principal is still uncommitted (`details.active_rotation_id`) |
| `optimistic_lock_conflict` | 409 | A concurrent writer (another call, or the overlap sweep) already changed the row's `record_version` (`details.expected_version`) |
| `credential_replay_revoked` | 409 | TS-1's `rotation_id` replay found the row it would replay, but that credential has since been revoked — additive to §17, not a misleading `502` (TS-D13) |
| `principal_revoked` | 422 | TS-1/TS-2 against a principal whose registration itself has been revoked (offboarded tenant) |
| `secret_store_unavailable` | 502 | OpenBao write/delete failed — material-first ordering means **nothing** was committed |
| `db_unavailable` | 503 | Postgres connectivity/resource exhaustion, classified by positive SQLSTATE identification only (class `08`/`53`/`57`/`58`, or a closed pool) — additive to §17, distinct from a caller's business error (TS-D16) |

The `db_unavailable` and `credential_replay_revoked` codes are explicitly
commented in `internal/core/domain/errors.go` as **additive, not frozen**
— the other seven codes above are the frozen §17 taxonomy and cannot
change in place.

### 9. Optimistic locking

`service_account_principals` and `service_account_credentials` both carry
`record_version`, bumped by the `touch_row()` DB trigger (§4.5) on any
real change — a no-op update never advances it. A `409
optimistic_lock_conflict` returns the current `expected_version` in
`details` so the caller can re-read and retry; there is no
client-supplied expected-version field on TS-1/TS-2/TS-4's write bodies —
conflicts surface from concurrent writers hitting the same row (another
TS-2 call racing the overlap sweep, say), not from a caller-echoed
version. TS-2 and the overlap sweep both re-check after a conflict and
report the already-achieved idempotent outcome rather than a false
failure when the "conflict" was actually two actors converging on the
same result (TS-D13).

### 10. Rate limits

**Every internal route (TS-1..TS-6) applies no inbound rate limiting of
its own** — all callers are trusted in-mesh backends behind a
NetworkPolicy allow-list, not external/public traffic. The one exception
is the EXT-6 JWKS route, which is deliberately unauthenticated by header
(Keycloak's own outbound fetch carries no caller identity) and so has no
caller-identity gate to lean on: it carries its own process-wide
token-bucket rate limiter (`JWKS_RATE_LIMIT_RPS`/`JWKS_RATE_LIMIT_BURST`,
default 20/40, TS-D15) to bound OpenBao read-amplification if it's hit
repeatedly.

The only *outbound* rate limiting in this codebase is the Realm
Provisioner's own `KEYCLOAK_RATE_LIMIT_RPS` on its Keycloak Admin API
calls — this service never calls Keycloak (TS-INV-1) and has no
equivalent outbound limiter of its own; its one outbound HTTP dependency
(RP-17, `internal/adapter/outbound/realmprovisioner`) instead retries a
bounded number of times on a plausibly-transient failure (TS-D15) rather
than rate-limiting.

### 11. Background reconcilers you may observe

`cmd/rotator` runs an overlap-expiry sweep and an orphan-material
reconciler on a cron schedule; both are read-only from any other service's
perspective — they only ever touch this service's own rows and its own
OpenBao paths, plus (since TS-D15) a call to the Realm Provisioner's RP-17
after every automatic revoke. `cmd/scheduler` (§16 TSQ-6 Resolved,
TS-D14) runs on its own cron schedule and *is* visible to other
services: it calls TS-1 automatically for any principal past its
`next_rotation_at`, emitting the same `…CredentialIssued`/`…CredentialRotated`
events an operator-triggered rotation would, then calls RP-17 itself.

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

| Command | Description |
|---|---|
| `make setup` | Copy `.env-example` → `.env` if missing; install `.githooks/pre-commit` |
| `make install-hooks` | Install `.githooks/pre-commit` (also run by `make setup`) |
| `make tidy` / `make fmt` / `make fmt-check` / `make vet` | Go basics; `fmt-check` mirrors CI, does not modify files |
| `make lint` | `golangci-lint` (via `go tool`) |
| `make gates` | The 5 invariant gates: `no-gocloak`, `no-secret-log`, `set-local-only`, `gincommon-obs`, `metrics-taxonomy` |
| `make mod-verify` | `go mod verify` |
| `make vuln-check` | `govulncheck` on `./internal/...`/`./pkg/...` |
| `make sast` | `gosec` Go-code SAST — distinct from `vuln-check` (dependency CVEs) and the release pipeline's Trivy scan (container/OS CVEs) |
| `make test` | Unit + contract + Postgres + integration suites, in parallel (Docker required) |
| `make test-ci` | Same, with `-race` + merged coverage (what CI runs) |
| `make test-unit` | Unit tests only, no Docker |
| `make test-contract` | Event-contract tests, no external services |
| `make test-postgres` | Postgres + RLS integration (testcontainers-go) |
| `make test-integration` | Cross-layer (Postgres + OpenBao via testcontainers) |
| `make test-e2e` | End-to-end tests |
| `make test-smoke` | CI-only image gate: size ≤200MB + startup-gate check (`IMAGE_TAG`+`BINARY` required) |
| `make race` | All four suites with `-race`, no coverage merge |
| `make run` / `make run-consumer` / `make run-scheduler` | Run `cmd/server` / `cmd/consumer` (`APP_PORT=8081`) / one `cmd/scheduler` scan pass (`METRICS_PORT=9092`) natively |
| `make build` | Compile all four binaries to `bin/` |
| `make cover` / `make cover-func` | Coverage HTML report / per-function summary |
| `make ci` | `tidy` + `fmt-check` + `vet` + `lint` + `gates` + `test-ci` + `build` |
| `make docker-up` / `make docker-down` | Start/stop Postgres + PgBouncer + Floci + Floci UI + OpenBao (infra only, no app containers) |
| `make docker-run-app` | Build + run the containerized server+consumer (requires `.go_private_token`) |
| `make docker-run-rotator` / `make docker-run-scheduler` | Run the rotator / scheduler once via Docker, then exit (requires `.go_private_token`) |
| `make extract-schemas` | Derive `internal/adapter/outbound/eventbus/schemas/*.json` from `api/asyncapi.yaml` |
| `make swag` | Generate Swagger docs into `docs/swagger/` from handler annotations — re-run and commit whenever they change |
| `make swag-check` | Fails if regenerating Swagger docs would change `docs/swagger/` (CI drift gate) |
| `make schema-pull` | Pull the `platform-schemagov` Docker image (`0.4`) |
| `make schema-validate` | Validate AsyncAPI + this service's 5 event schemas — no AWS credentials needed |
| `make schema-diff CURRENT=… PROPOSED=…` | Show compatibility diff between two schema files |
| `make schema-register` | Register this service's 5 event schemas to the Glue registry (requires AWS) |
| `make schema-verify` | Pre-deploy check that each of this service's 5 schema definitions is registered and `AVAILABLE` — the same lookup the pod runs at startup |
| `make schema-prune` | Dry-run: list orphaned Glue schemas (`EXECUTE=true` to archive+delete) |
| `make pin-base-images` | Fetch and pin the current SHA digests for the Dockerfile's base images |
| `make godoc` | Serve package documentation locally (pkgsite, `:8080`) |
| `make docs-serve` | Runs the server, then prints the three docs URLs (`/`, `/swagger`, `/asyncapi`) |
| `make clean` | Remove `bin/`, `.coverage/`, and coverage artifacts |
| `make help` | Print the same command list this table documents, from the Makefile itself |

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
| `make run` panics with `resolve glue schema ... by definition` | This build's schema definition isn't registered in floci's Glue registry (e.g. a schema changed since floci started) | Recreate floci so `init-floci.sh` re-registers, or unset `GLUE_REGISTRY_NAME` for plain JSON |
| A `TenantMembershipsPurged` lands in `tenant-lifecycle-tokensvc-q-dlq` immediately | Permanent reject — its `DLQReason` message attribute is `schema_violation` (payload failed `tenant_memberships_purged.json`, e.g. no `tenant_id`); `iam_token_service_consumed_schema_violations_total` rose | Fix the producer payload, or `api/asyncapi.yaml` + `make extract-schemas` if our schema is wrong, then redrive — that tenant's cascade has not run |
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
- `cmd/consumer` — the inbound pipeline: a Glue-framed `TenantMembershipsPurged`
  decoded by the real consumer, consumed-schema validation, straight-to-DLQ
  routing (`dlq_test.go`, `inbound_schema_test.go`).
- `test/integration/glue_codec_test.go` — floci Glue: versions resolved by
  definition, never "latest".

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
| `RECONCILER_DATABASE_URL` | direct port | `serviceaccount_reconciler` (`BYPASSRLS`, `SELECT`-only) pool for `cmd/rotator` and `cmd/scheduler` |
| `OPENBAO_ADDR`, `OPENBAO_ROLE`, `OPENBAO_KV_MOUNT` | `http://localhost:8210`, `iam-token-service`, `iam` | OpenBao Kubernetes-auth login + KV v2 mount |
| `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL` | `ap-south-1`, `test`/`test`, Floci endpoint | AWS SDK config (Floci locally, real AWS in deployed environments) |
| `GLUE_REGISTRY_NAME` | `iam-serviceaccount-events` | Glue Schema Registry name. Set → `cmd/server` resolves each produced schema's version UUID once at startup by definition (`glue:GetSchemaByDefinition`); a schema not registered yet fails startup until `schema-registry.yml` registers it. Unset → `NoopCodec` (plain JSON). `cmd/consumer` needs no Glue config — it only strips inbound headers |
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
| Cross-tenant enumeration role | `serviceaccount_reconciler` — `BYPASSRLS` but grantless beyond `SELECT`; used only by `cmd/rotator` and `cmd/scheduler` (every resulting write still goes through RLS-scoped `serviceaccount_app`) |
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

### Container image — four binaries

| Binary | Entrypoint | Purpose |
|---|---|---|
| `cmd/server` | `/iam-token-service` (default) | HTTP API (TS-1..TS-6 + EXT-6 JWKS route) + outbox runner |
| `cmd/consumer` | `/iam-token-service-consumer` | Offboarding SQS subscriber |
| `cmd/rotator` | `/iam-token-service-rotator` | Overlap-expiry sweep + orphan-material reconciler + prune, run-to-completion |
| `cmd/scheduler` | `/iam-token-service-scheduler` | Automatic cadence-driven rotation scan + RP-17 key-refresh call (§16 TSQ-6 Resolved, TS-D14), run-to-completion |

### Helm chart

`deploy/helm/` — `Deployment` × 2 (server, consumer, each `replicaCount: 2`
for HA, not load), `CronJob` × 2 (rotator and scheduler — each
`activeDeadlineSeconds: 240s`, deliberately shorter than its 5-minute
schedule so a run never eats into the next scheduled tick under
`concurrencyPolicy: Forbid`), `NetworkPolicy`
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

### Networking reference

| What you want to reach | Hostname inside Docker (compose network) | Hostname from the host |
|---|---|---|
| This service's API | `server:8080` | `localhost:8080` |
| This service's `/metrics` | `server:9090` | `localhost:9090` |
| PostgreSQL | `postgres:5432` | `localhost:5535` |
| PgBouncer | `pgbouncer:5432` | `localhost:5536` |
| Floci (SNS/SQS/Glue) | `floci:4566` | `localhost:4568` |
| Floci UI | `floci-ui:4500` | `localhost:4502` |
| OpenBao | `openbao:8200` | `localhost:8210` |

### Calling the service from another container

```bash
# From inside another container on the same compose network — TS-1,
# issue/rotate, the crux endpoint. Only ever returns the plaintext
# private key to THIS caller, and only once (TS-INV-2).
curl -X POST http://server:8080/api/v1/internal/tenants/<tenant-uuid>/service-accounts/<principal-uuid>/credentials \
  -H "Content-Type: application/json" \
  -H "x-user-id: 00000000-0000-0000-0000-0000000000a1" \
  -H "x-tenant-id: <tenant-uuid>" \
  -d '{"rotation_id":"'"$(uuidgen)"'","overlap_seconds":300}'
```

### Integrating into another project's `docker-compose.yml`

```yaml
services:
  iam-token-service:
    image: ghcr.io/bcbp-solutions-fzc-llc/iam-token-service:latest
    ports:
      - "8080:8080"
      - "9090:9090"
    environment:
      APP_ENV: dev
      PG_HOST: iam-postgres
      PG_PORT: "5432"
      PG_USER: serviceaccount_app
      PG_PASSWORD: devpassword
      PG_DBNAME: serviceaccount
      PG_SSLMODE: disable
      MIGRATION_DATABASE_URL: postgres://serviceaccount_migrator:devpassword@iam-postgres:5432/serviceaccount?sslmode=disable
      RECONCILER_DATABASE_URL: postgres://serviceaccount_reconciler:devpassword@iam-postgres:5432/serviceaccount?sslmode=disable
      OPENBAO_ADDR: http://iam-openbao:8200
      OPENBAO_ROLE: iam-token-service
      OPENBAO_KV_MOUNT: iam
      AWS_REGION: ap-south-1
      AWS_ENDPOINT_URL: http://iam-floci:4566
      AWS_ACCESS_KEY_ID: test
      AWS_SECRET_ACCESS_KEY: test
      GLUE_REGISTRY_NAME: iam-serviceaccount-events
      SQS_OFFBOARDING_QUEUE_URL: http://iam-floci:4566/000000000000/tenant-lifecycle-tokensvc-q
    depends_on:
      iam-postgres:
        condition: service_healthy
```

This starts `cmd/server` only — `cmd/consumer`, `cmd/rotator`, and
`cmd/scheduler` are the same image with a different `command`/`entrypoint`
(see "Container image — four binaries" above); `cmd/rotator` and
`cmd/scheduler` additionally need `REALM_PROVISIONER_BASE_URL` (RP-17) to
do anything useful. The chart-level source for the image path is
`deploy/helm/values.yaml`'s `image.repository`
(`ghcr.io/bcbp-solutions-fzc-llc/iam-token-service`); no `v*` release tags
exist yet — pin `main`-branch images by digest, not `latest`.

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
| Called by | Realm Provisioner | TS-4 (register), TS-1 (issue/rotate) — RP discards the returned private-key plaintext immediately and calls Keycloak's `ClearServiceAccountKeysCache` (RP-17) instead, so Keycloak re-fetches this service's own JWKS (EXT-6) |
| Called by | Org & Membership, operators | TS-3 (read metadata), TS-5 (find-by-sub, AUTH-9 non-member defense-in-depth) |
| Called by | Workflow Service | TS-6 (read the tenant's automation subject for connector callbacks, TS-D17) |
| Called by | Keycloak | The EXT-6 JWKS route — the one unauthenticated-by-header, fully public route |
| Called by | Operators, O&M tooling, `cmd/scheduler` | TS-1 (rotate), TS-2 (revoke) — `cmd/scheduler` automatically rotates any principal past its `next_rotation_at` (LLD §16 TSQ-6 Resolved, TS-D14), then calls RP-17 itself so Keycloak re-fetches the JWKS. `cmd/rotator` never calls TS-1, but it does call RP-17 after every automatic revoke (TS-D15) |
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
| [`docs/iam-lld-token-service.md`](docs/iam-lld-token-service.md) | Full LLD (rev 1.3, Approved) — §16 open questions, §17 error taxonomy, §25 frozen name inventory |
| [`docs/observability-registry-proposals.md`](docs/observability-registry-proposals.md) | Tier-1/Tier-2 metric registry submissions pending ratification |

---

## License / ownership

Internal service, owned by the BCBP Solutions IAM/Platform team
(`platform@bcbpsolutions.com`). Not for external distribution.
