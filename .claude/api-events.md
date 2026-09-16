# API and events

No caching layer in this service (unlike some IAM siblings — nothing here
to document under that heading). API conventions, endpoint catalogue,
status codes, and the event contract, at LLD §5/§7 depth.

## 5.1 API conventions

- Every route is under `/api/v1/internal/*` — **there is no
  public/tenant-facing surface at MVP** (unlike siblings, which have
  public route classes too). A breaking change ships under a new prefix
  (`/api/v2/internal`) with both served during deprecation; additive
  changes stay on `v1`.
- Middleware chain: `PanicRecovery → RequestID → Tracing → CorrelationHeaders → Metrics → Logging → RequireAuth → ContextMiddleware`,
  then this service's own `GUCBridgeMiddleware`. `/healthz`/`/readyz`
  registered before auth.
- Identity comes from mesh-injected headers (`x-user-id`, `x-tenant-id`) —
  **no JWT parsing** in this service; `RequireAuth` only validates
  header presence/shape.
- `tenant_id` for every query is the GUC, never a body/path parameter for
  authorization purposes — the `:id` path segment is validated to equal
  the `x-tenant-id` GUC before any checkout (mismatch → `403`).
- Mutation responses always include `record_version` (optimistic-lock
  round-trip token). No response body ever includes a stored secret
  except TS-1's freshly-generated plaintext, exactly once.

## 5.2 Authorization rules

Single model, no per-user/role decision (TS-INV-4 — the automation
principal carries no roles). Every route requires the reserved system
principal `iam-system` (`00000000-0000-0000-0000-0000000000a1`) **and**
the target tenant's `x-tenant-id`. The system principal is accepted only
on `/api/v1/internal/*`.

## 5.3 Endpoint catalogue

| # | Method & path | Purpose | Idempotent |
|---|---|---|---|
| TS-4 | `POST /api/v1/internal/tenants/:id/service-accounts` | Register the principal after RP mints the Keycloak client | On `(tenant_id, principal_type)` |
| TS-1 | `POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials` | Issue or rotate — returns plaintext once | Per `rotation_id` |
| TS-2 | `POST /api/v1/internal/tenants/:id/service-accounts/:principal_id/credentials/:version/revoke` | Revoke one version | Yes (re-revoke is a no-op) |
| TS-3 | `GET /api/v1/internal/tenants/:id/service-accounts/:principal_id` | Read metadata — never a secret | Read |
| TS-H | `GET /healthz`, `GET /readyz` | Liveness/readiness | Read |
| TS-D | `GET /asyncapi`, `GET /asyncapi.yaml`, `GET /swagger/*any` | Rendered/raw contracts (gated) | Read |

No listing endpoint (`GET .../service-accounts`) at MVP — exactly one
automation principal per tenant, read directly by TS-3.

## 5.5 Status codes (full taxonomy in `internal/core/domain/errors.go`)

| `error` | HTTP | `details` |
|---|---|---|
| `missing_identity_headers` | 401 | — |
| `principal_not_found` | 404 | — |
| `rotation_in_flight` | 409 | `active_rotation_id` |
| `optimistic_lock_conflict` | 409 | `expected_version` |
| `principal_revoked` | 422 | — |
| `secret_store_unavailable` | 502 | — |
| `invalid_request` | 400 | `field` |
| `db_unavailable` | 503 | — (additive, not in the original frozen §17 list) |
| `credential_replay_revoked` | 409 | `version` (additive, LLD TS-D13) |

## 7.1 Inbound events

**One active subscription:** `TenantMembershipsPurged` on
`tenant-lifecycle-tokensvc-q` (DLQ `-dlq`, `maxReceiveCount=5`), filtered
via an SNS filter policy so no other `iam.tenant.events` type is
delivered. Does **not** consume `MembershipRevoked` — a per-user
membership change never touches a service-account principal (non-member
by construction, O&M AUTH-9).

## 7.3/7.4 Outbound events — single producer, transactional outbox

`iam-serviceaccount-events` (Glue registry, single producer — no
`RoutingPublisher` needed since there's only one topic). Every credential
state transition writes its event to `outbox_events` in the **same
transaction** as the business write (EVT-1) — never published for a
rolled-back write, never missing for a committed one. Enqueue-time
validation is schema-check-only against plain JSON
(`ValidatingCodec.Encode`, encoded bytes discarded — only the validation
side effect matters); Glue wire-encoding happens later, at publish time
(`GlueCodec`, via the outbox runner's `events.WithCodec` hook), so
`outbox_events.payload` stays human-readable.

## 7.5 Published events (5, frozen §25)

| Event | Emitted when | Consumers |
|---|---|---|
| `ServiceAccountRegistered` | TS-4 registers a principal | Audit |
| `ServiceAccountCredentialIssued` | TS-1 first credential (`version=1`) | Audit |
| `ServiceAccountCredentialRotated` | TS-1 rotation (`version>1`) | Audit |
| `ServiceAccountCredentialRevoked` | TS-2, overlap sweep, or offboarding revoke | Audit |
| `ServiceAccountRevoked` | Principal fully revoked (offboarding) | Audit |

**No payload ever carries a secret** (TS-INV-2/EVT-2) — a negative
contract test asserts this. No downstream authorization consumer
subscribes (audit-only by construction, TS-INV-4).

## Event invariants

| # | Invariant |
|---|---|
| EVT-1 | Every credential state transition emits exactly one event, in the same transaction as the state write |
| EVT-2 | No event payload carries a secret or secret-derived value |
| EVT-3 | Redelivery is safe — each envelope id (UUID v7) is stable, Audit dedups on it |
| EVT-4 | Audit visibility never depends on a live call — the outbox decouples emission from Audit availability |

## Idempotency and ordering

At-least-once publishing; no ordering requirement — each credential
operation is idempotent per `rotation_id` (TS-1) or per version (TS-2),
and payloads are self-contained snapshots (not deltas), so out-of-order or
redelivered events converge to the correct latest state. A consumer that
rejects a Glue-decode failure routes it to its own DLQ rather than
retrying indefinitely.
