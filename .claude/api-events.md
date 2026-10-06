# API and events

No caching layer in this service (unlike some IAM siblings — nothing here
to document under that heading). API conventions, endpoint catalogue,
status codes, and the event contract, at LLD §5/§7 depth.

## 5.1 API conventions

- Every route is under `/api/v1/internal/*` — **there is no
  tenant-facing surface at MVP**. A breaking change ships under a new
  prefix (`/api/v2/internal`) with both served during deprecation;
  additive changes stay on `v1`.
- Middleware chain (`internal/adapter/inbound/http/router.go`): a 1 MiB
  request-body cap → gincommon `ObservabilityMiddlewares` (`PanicRecovery
  → RequestID → Tracing → CorrelationHeaders → Metrics → Logging`, then
  `TimeoutMiddleware` from `Config.RequestTimeout` = 30s, so a timed-out
  request is recorded as the 503 the client gets). `/healthz`, `/readyz`,
  the docs routes and the JWKS route are registered outside the
  protected group. The `/api/v1/internal` group then runs
  `RequireIdentityHeaders → RequireAuth → ContextMiddleware →
  GUCBridgeMiddleware → RequireJSONContentType`, and `/tenants/:id` adds
  `RequireTenantPathMatch`.
- Identity comes from mesh-injected headers (`x-user-id`, `x-tenant-id`) —
  **no JWT parsing**. `RequireIdentityHeaders` returns `401
  missing_identity_headers` for a missing, repeated or non-UUID value and
  deletes any `x-tenant-roles` header. `GUCBridgeMiddleware` requires
  `x-user-id` to be the iam-system UUID (else `401
  missing_identity_headers`) and binds both into the pgcommon GUC set.
- `tenant_id` for every query is the GUC. The `:id` path segment must be a
  UUID (`400 invalid_request`) equal to `x-tenant-id` (`403
  tenant_path_mismatch`) before any DB checkout.
- A POST with a body must be `application/json` (`415
  unsupported_media_type`).
- Mutation responses include `record_version`. No response body ever
  includes a stored secret except TS-1's private key — fresh, or replayed
  within the replay window. TS-1 responses carry `Cache-Control:
  no-store` and `Pragma: no-cache`.
- Error body: `{error, status, trace_id, request_id, details}`.

## 5.2 Authorization rules

Single model, no per-user/role decision (TS-INV-4 — the automation
principal carries no roles). Every TS route requires the reserved system
principal `iam-system` (`00000000-0000-0000-0000-0000000000a1`) as
`x-user-id` **and** the target tenant as `x-tenant-id`. Because any
in-mesh pod could set that header, outside local/dev/test the Helm chart
requires the Istio `AuthorizationPolicy` that limits callers by namespace:
mesh namespaces on every route, Keycloak namespaces on the JWKS route
only, Workflow namespaces on TS-6 only.

## 5.3 Endpoint catalogue

Paths are relative to `/api/v1/internal/tenants/:id`.

| # | Method & path | Purpose | Success | Idempotent |
|---|---|---|---|---|
| TS-4 | `POST /service-accounts` | Register the principal after RP mints the Keycloak client. `principal_sub` (UUID) and `keycloak_client_id` (`platform-automation` or `platform-automation-<tenant_id>`) required. A repeat with a different sub/client id (RP-3/RP-4 re-mint) updates the row in place | `201` created, `200` repeat/update | On `(tenant_id, principal_type)` |
| TS-1 | `POST /service-accounts/:principal_id/credentials` | Issue or rotate — returns the PEM private key. Body `rotation_id` (UUID, required), `overlap_seconds` (omitted → `ROTATION_DEFAULT_OVERLAP_SECONDS`; clamped to `[0,900]`) | `201` new, `200` replay | Per `rotation_id`, within `ROTATION_REPLAY_WINDOW` |
| TS-2 | `POST /service-accounts/:principal_id/credentials/:version/revoke` | Revoke one version (custody half only, TS-INV-7) | `200` | Yes (re-revoke is a no-op `200`) |
| TS-3 | `GET /service-accounts/:principal_id` | Principal metadata + every credential's version/status/path/`expires_at`/cadence — never a secret | `200` | Read |
| TS-5 | `GET /service-accounts?principal_sub=<uuid>` | Find by Keycloak `sub` (AUTH-9, TS-D16) — identity/status only; used by org-membership's non-member defense-in-depth check | `200` / `404` | Read |
| TS-6 | `GET /service-accounts/platform-automation` | Read the tenant's automation principal (TS-D17) — the reverse of TS-5, returns `principal_sub` and `keycloak_client_id`; used by the Workflow Service's connector workers. `principal_sub` changes on an RP-3/RP-4 re-mint, `principal_id` doesn't | `200` / `404` | Read |
| EXT-6 | `GET /service-accounts/platform-automation/jwks.json` | Public JWK Set for Keycloak's client-jwt authenticator. No identity headers; rate-limited | `200` | Read |
| TS-H | `GET /healthz`, `GET /readyz` | Liveness / readiness (database, OpenBao, outbox; 2s per check; 503 while draining) | | Read |
| TS-D | `GET /asyncapi`, `/asyncapi.yaml`, `/swagger/*any` | Contracts; always on in local/dev/test, elsewhere only with `DOCS_ENABLED=true` and a bearer `DOCS_AUTH_TOKEN` (the server refuses to start without it) | | Read |

`GET /service-accounts` is not a listing endpoint — `principal_sub` is
required (`400` otherwise). Exactly one automation principal per tenant,
read by TS-3 (by id), TS-5 (by sub) or TS-6 (by its frozen name).

### JWKS route (EXT-6)

- Keys: every `active` and `rotating` credential of the tenant's active
  `platform_automation` principal whose overlap has not expired; `kid` =
  the credential row id, `RS256`, `use: sig`. Unknown tenant or no
  principal → `200 {"keys": []}`.
- An unreadable key is skipped and counted in
  `iam_token_service_jwks_key_errors_total`. If the active key (or every
  key) is unreadable the response is `503 jwks_keys_unavailable`; a set
  missing only rotating keys is still `200`.
- Rate limits (all token buckets, `429 rate_limited`): a per-tenant
  bucket (5 rps / burst 10, LRU of 10,000 tenants) for every request,
  then the global bucket (20/40) for a *known* tenant or the shared
  unknown-tenant bucket (2/5) for any other. Known = the tenants with an
  `active`/`rotating` credential (reloaded every
  `JWKS_KNOWN_TENANTS_REFRESH`, 15s, over the reconciler pool) plus
  tenants served keys since the last reload. Counted in
  `jwks_rate_limited_total` and `jwks_rate_limited_by_bucket_total{bucket}`.
- Headers: `Cache-Control: no-cache`, `X-Content-Type-Options: nosniff`.

## 5.5 Status codes (`internal/core/domain/errors.go`, `http/errors.go`)

| `error` | HTTP | `details` | Status |
|---|---|---|---|
| `missing_identity_headers` | 401 | — | frozen §17 |
| `principal_not_found` | 404 | — | frozen |
| `rotation_in_flight` | 409 | `active_rotation_id` (best effort, also on the `55P03` lock-timeout path) | frozen |
| `optimistic_lock_conflict` | 409 | `expected_version` | frozen |
| `principal_revoked` | 422 | — | frozen |
| `secret_store_unavailable` | 502 | — | frozen |
| `invalid_request` | 400 | `field`, `reason` | frozen |
| `db_unavailable` | 503 | — | additive (SQLSTATE class 08/53/57/58 or closed pool, TS-D16) |
| `credential_replay_revoked` | 409 | `version` | additive (TS-D13) |
| `credential_replay_expired` | 409 | `version` | additive (TS-D22) |
| `jwks_keys_unavailable` | 503 | — | additive, JWKS route (TS-D23) |
| `rate_limited` | 429 | — | additive, JWKS route |
| `tenant_path_mismatch` | 403 | — | additive |
| `unsupported_media_type` | 415 | — | additive |
| `internal_error` | 500 | — | any unclassified error |

## 7.1 Inbound events

**One active subscription:** `TenantMembershipsPurged` on
`tenant-lifecycle-tokensvc-q` (DLQ `-dlq`, `maxReceiveCount=5`), filtered
via an SNS filter policy so no other `iam.tenant.events` type is
delivered. Does **not** consume `MembershipRevoked` — a per-user
membership change never touches a service-account principal (O&M AUTH-9).

**Consumer pipeline** (`cmd/consumer`), outermost first: DLQ router →
cascade metric (`offboarding_cascade_total{result}`) → `validateConsumed`
→ `OffboardingConsumer.Handle`, on an SQS consumer with
`eventbus.GlueDecoder` (O&M publishes Glue-encoded; the decoder strips
the header, no registry lookup).

- **Schema check:** the payload is validated against the embedded
  `tenant_memberships_purged.json` (`tenant_id` required). A violation
  increments `consumed_schema_violations_total` and is a permanent reject.
  A type with no embedded schema passes through.
- **Envelope id:** a `TenantMembershipsPurged` whose id is missing or not
  a UUID is a permanent reject (`invalid_envelope_id`, TS-D22) — the
  tenant's erasure has not run, so it must be redriven, not dropped.
- **Unknown event type:** acked, never retried (`ackUnknown`: metric with
  `event_type` bounded to a closed set, else `other`; info log; claimed in
  `processed_events`). With a bad envelope id it is just logged and acked.
- **DLQ router:** a permanent reject (`schema_violation` or
  `invalid_envelope_id`) is sent straight to the DLQ (URL derived from the
  queue's `RedrivePolicy`; `DLQReason`/`EventType` message attributes),
  counted in `consumer_dlq_rejects_total{reason}`, and acked. If the DLQ
  URL can't be resolved at startup or the send fails, the message falls
  back to normal SQS redrive.
- **Dedup** is platform-events' `pkg/inbox` over `processed_events`
  (`consumer='tenant_offboarding'`, keyed on the envelope id, TS-D19): the
  claim, the cascade's OpenBao deletes and Postgres writes, and its
  `ServiceAccountRevoked` outbox events run in one transaction, so a
  redelivered or concurrent copy is acked as a no-op and a failed cascade
  leaves the event unclaimed for redelivery. Duplicates count on
  `processed_events_duplicates_total` (the inbox itself records
  `platform_duplicate_messages_total`).
- **Shutdown:** on SIGTERM `/readyz` returns 503 for
  `SHUTDOWN_DRAIN_DELAY`, then the consumer drains (`SQS_DRAIN_TIMEOUT`).
  If the consume loop exits on its own the process exits 1 so the pod
  restarts.

## 7.3/7.4 Outbound events — single producer, transactional outbox

`iam-serviceaccount-events` (Glue registry, single producer). Every
credential state transition writes its event to `outbox_events` in the
**same transaction** as the business write (EVT-1). Each schema's Glue
version is resolved once at startup by definition
(`glue:GetSchemaByDefinition`, must be `AVAILABLE`) — never "latest", no
refresher, no per-event Glue call. Enqueue-time validation is
schema-check-only against plain JSON (`ValidatingCodec`); Glue
wire-encoding happens at publish time (`GlueCodec` on the SNS publisher),
so `outbox_events.payload` stays human-readable. With no
`GLUE_REGISTRY_NAME` the codec is a no-op; with no `SNS_TOPIC_ARN` the
publisher is a no-op (both required outside dev).

## 7.5 Published events (5, frozen §25)

| Event | Emitted when | Consumers |
|---|---|---|
| `ServiceAccountRegistered` | TS-4 creates a principal, or updates it for a re-minted client (RP-3/RP-4; new `principal_sub`, same `principal_id`); not on a no-op repeat | Audit; Workflow may use it to refresh a cached TS-6 sub |
| `ServiceAccountCredentialIssued` | TS-1 when the principal has no active credential | Audit |
| `ServiceAccountCredentialRotated` | TS-1 rotation (`prior_version`, `expires_prior_at`) | Audit |
| `ServiceAccountCredentialRevoked` | TS-2, the TS-1 opportunistic sweep, or the rotator sweep | Audit |
| `ServiceAccountRevoked` | Offboarding cascade, one per principal | Audit |

**No payload ever carries a secret** (TS-INV-2/EVT-2) — a contract test
(`test/contract/no_secret_payload_test.go`) asserts this. No downstream
authorization consumer subscribes (audit-only, TS-INV-4).

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
and payloads are self-contained snapshots, so out-of-order or redelivered
events converge. The outbox runner moves an event that exhausts
`OUTBOX_MAX_ATTEMPTS` to `outbox_dead_letters`.
