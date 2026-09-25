# Request flows, concurrency, and failure handling

Prose numbered-step walkthroughs (matching `docs/lld/iam-lld-token-service.md` §8
numbering) — meant to be read directly, not rendered. Visual sequence
diagrams for the same flows: root `ARCHITECTURE.md` (mermaid, one per flow).

## 8.1 TS-4 — register

1. Realm Provisioner mints the `platform-automation` Keycloak client
   (RP-1/RP-2), then calls `POST /tenants/:id/service-accounts` with
   `principal_sub`/`keycloak_client_id`.
2. `PrincipalRepository.Register` does `INSERT ... ON CONFLICT (tenant_id, principal_type) DO NOTHING`
   then reads back the row — idempotent on `(tenant_id, principal_type)`.
   A repeat call returns the existing principal as `200`, never a
   duplicate.
3. Emits `ServiceAccountRegistered`.
4. TS-1 against an unknown `principal_id` is `404 principal_not_found` —
   TS-4 must precede the first TS-1 for a principal.

## 8.2 TS-1 — issue/rotate (the crux flow)

1. `IssueOrRotate` checks `FindByRotationID` first — a replay under the
   same `rotation_id` skips straight to step 5 (no new material/row/event).
2. **Stale-replay classification (LLD TS-D13):** if the found row's status
   is `revoked`, return `409 credential_replay_revoked` immediately — do
   not attempt an OpenBao read against material that's already gone (that
   would surface as a misleading `502`). If `active`/`rotating`, replay
   returns that version's real secret — correct idempotency-key semantics,
   not a bug, even though the row may have been superseded by a later
   rotation since.
3. New rotation: `generate()` (crypto/rand) → `secrets.Write` to OpenBao at
   `v{n+1}` — **before** any Postgres write. A crash here leaves nothing
   committed; a crash after this write but before the commit below leaves
   a reclaimable orphan (§8.6 reconciler).
4. `RunInTx`: demote the prior `active` row to `rotating` (`expires_at = now()+overlap`)
   **before** inserting the new `active` row — `uq_sac_one_active` is a
   plain, immediately-checked partial unique index; insert-then-demote
   would violate it.
5. `Insert` uses a `SAVEPOINT`/`ROLLBACK TO SAVEPOINT` around the write so
   a genuine concurrent `rotation_id` collision (`23505`) can still run the
   `active_rotation_id` enrichment lookup afterward — without the
   savepoint, Postgres aborts the whole transaction on the first failed
   statement, so that lookup could never succeed.
6. Enqueue `ServiceAccountCredentialIssued` (version 1) or `...Rotated`
   (version > 1, `prior_version`/`expires_prior_at` set) in the same tx.
7. Return the plaintext once. Realm Provisioner applies it to the Keycloak
   client and discards it — this service never writes Keycloak.

## 8.3/8.4 TS-2 — revoke, and the overlap-expiry sweep

1. `FindByVersion` → if already `revoked`, return the idempotent `200`
   no-op immediately.
2. `secrets.Delete` (OpenBao) **before** the Postgres write — material-first,
   same discipline as issue/rotate in the destructive direction. Delete is
   a no-op on an already-missing path, so retrying the whole operation
   after a partial failure is always safe.
3. `RunInTx`: `Update` (optimistic-lock guarded on `record_version`) →
   `revoked`, enqueue `ServiceAccountCredentialRevoked`.
4. **Concurrent-revoke race (LLD TS-D13):** if step 3's `Update` hits
   `optimistic_lock_conflict` (another TS-2 call, or the overlap-expiry
   sweep, revoked this exact row first), re-fetch the row; if it's now
   `revoked`, return that outcome as success instead of surfacing `409` —
   the caller's desired end state was already reached.
5. **TS-INV-7 (two-halves):** this alone does not stop the secret
   authenticating at Keycloak. Complete only where a Keycloak-side change
   is already implied (overlap-expiry's own Keycloak grace TTL, or
   offboarding's realm deletion) — otherwise the caller must pair this
   with the Realm Provisioner action.

The overlap-expiry sweep (`cmd/rotator/sweep.go`, cron-scheduled) runs the
same revoke discipline directly against `postgres`/`openbao` (it cannot
import `service` — RLS-7): enumerate expired `rotating` rows via the
BYPASSRLS reconciler pool, then revoke each one through a normal
RLS-scoped transaction bound to that row's own tenant. A write-time
`optimistic_lock_conflict` here is reclassified as `Skipped`, not
`Failed` — same benign-race handling as TS-2, so
`rotation_sweep_total{result="failed"}` doesn't page on-call for a race
that converged correctly.

## 8.4 Offboarding cascade (`TenantMembershipsPurged`)

0. **Before `Handle`** (`cmd/consumer` pipeline): `GlueDecoder` strips the
   Glue header (O&M publishes this event Glue-encoded); the payload is
   validated against the embedded `tenant_memberships_purged.json`. A
   violation (e.g. no `tenant_id`) never reaches the steps below — it is
   counted (`consumed_schema_violations_total`) and sent straight to the
   DLQ (`DLQReason=schema_violation`), then acked.
1. Envelope-id validation: missing or non-UUID → **acked, not retried**
   (logged, no side effect). Unknown event type → `ackUnknown` (metric +
   info log + marked processed) — never DLQ-storms this queue on a
   producer schema addition.
2. `IsProcessed` short-circuit on the envelope id — a redelivery of an
   already-handled message is a no-op ack.
3. Bind the GUC to this event's `tenant_id` + the system principal.
   `ListByTenant` → for each principal, `ListByPrincipal` to collect every
   credential's OpenBao path — **before** any delete.
4. Delete every collected OpenBao path — **before** the Postgres commit
   (§9.3/§15.2): once the principal row is gone, the §8.6 orphan
   reconciler (which enumerates the *live* registry) can never find this
   tenant's material again, so DB-first ordering could leak it forever.
5. `RunInTx`: `DeleteByTenant` (FK-cascades credentials) → `MarkProcessed`
   → enqueue `ServiceAccountRevoked` per principal.
6. **Partial-failure behavior (verified, not just assumed):** if OpenBao
   delete succeeds for credential 1 of 3 and fails on 2, `Handle` returns
   the error before any DB write — nothing commits, `processed_events` is
   never marked, and SQS redelivers the whole message (credential 1's
   re-delete is a no-op the second time). If retries exhaust
   `maxReceiveCount` and the message DLQs, the tenant's Postgres rows
   remain fully intact while some OpenBao material is already gone — the
   §8.6 reconciler still sees the live principal and correctly reports the
   missing entries as `missing_material` (page-worthy), rather than
   silently losing them.

## 8.6 Orphan-material reconciler (`cmd/rotator/orphan_reconciler.go`)

Enumerates the principal registry across all tenants (BYPASSRLS) with each
principal's committed credential versions, cross-references against
OpenBao `List` under each tenant/client prefix: an OpenBao path with no
matching committed version is `orphan_deleted` (reclaimed); a committed
version with no OpenBao entry is `missing_material` (page — irrecoverable
data loss for that version, never fabricated).

## 9. Concurrency, idempotency, failure — summary table

| Failure | Handling |
|---|---|
| OpenBao unavailable on TS-1 write | `502`; nothing committed (material-first ordering) |
| Crash after OpenBao write, before commit | Orphaned path below the committed max — reclaimed by §8.6 |
| Lost TS-1 response | Retry same `rotation_id` → same version + secret, no double-rotation |
| Different `rotation_id` mid-rotation | `409 rotation_in_flight` |
| Optimistic-lock conflict (non-race, e.g. stale client-side cache) | `409 optimistic_lock_conflict` |
| Concurrent-revoke race (TS-D13) | Re-checked, idempotent outcome returned, not `409`/counted as failed |
| Replay against a revoked version (TS-D13) | `409 credential_replay_revoked`, not a misleading `502` |
| Issue against a revoked principal | `422 principal_revoked` |
| Offboarding event redelivered | `processed_events` short-circuit, no-op |
| Offboarding payload fails its embedded schema | Straight to `-dlq` (`DLQReason=schema_violation`) + ack, cascade not run, critical alert; normal redrive if the DLQ can't be resolved or the send fails |
| Outbox relay lag / SNS outage | Queues in `outbox_events`; drains on recovery; audit delayed, never lost |

## 15. GDPR / data lifecycle

Tenant hard-delete flows entirely through the offboarding cascade above —
there is no separate "soft delete then purge later" path for this
service's data. A revoke's OpenBao delete (CUST-2) is a full KV v2
metadata delete, not a soft/recoverable one. Because plaintext lives only
in OpenBao, a Postgres restore never resurrects a secret; a
metadata/OpenBao divergence is reconciled by rotation (regenerate →
re-apply), never by reading a stale secret.
