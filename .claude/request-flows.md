# Request flows, concurrency, and failure handling

Prose numbered-step walkthroughs (matching `docs/lld/iam-lld-token-service.md` §8
numbering) — meant to be read directly, not rendered. Visual sequence
diagrams for the same flows: root `ARCHITECTURE.md` (mermaid, one per flow).

**The principal lock (TS-D21).** Every credential write for a principal
serializes on its `service_account_principals` row: TS-1, TS-2 and the
orphan reconciler take `SELECT … FOR UPDATE` (`LockForUpdate`), the
offboarding cascade locks all of the tenant's principal rows
(`LockByTenant`). A wait longer than `PG_LOCK_TIMEOUT` (Helm 2s) fails
with SQLSTATE `55P03`, mapped everywhere to `409 rotation_in_flight`
(`postgres/db.go`; TS-1/TS-2 add `details.active_rotation_id`, best
effort). Every OpenBao call made while holding the lock is bounded at 5s
(`inLockSecretWriteTimeout` / the rotator's `inLockSecretTimeout`) so it
can't outlast the other writers' lock wait. The overlap sweep does **not**
take the lock; it relies on `record_version` and re-reads instead.

## 8.1 TS-4 — register

1. Realm Provisioner mints the `platform-automation` Keycloak client
   (RP-1/RP-2), then calls `POST /tenants/:id/service-accounts` with
   `principal_sub` and `keycloak_client_id` (`platform-automation` or
   `platform-automation-<tenant_id>`).
2. `PrincipalRepository.Register` does `INSERT … ON CONFLICT (tenant_id,
   principal_type) DO UPDATE` — the update fires only if the sub or client
   id differ (an RP-3/RP-4 re-mint). Unchanged repeat → reads back the
   existing row. `201` on create, `200` otherwise; `principal_id` never
   changes.
3. Emits `ServiceAccountRegistered` on create or update, not on a no-op
   repeat.
4. TS-1 against an unknown `principal_id` is `404 principal_not_found` —
   TS-4 must precede the first TS-1.

## 8.2 TS-1 — issue/rotate (the crux flow)

1. Pre-checks without the lock: `rotation_id` must be set; principal
   exists (`404`) and is not revoked (`422 principal_revoked`);
   `FindByRotationID` — an already-committed `rotation_id` goes to the
   replay path (step 7) with no new material, row or event.
2. Clamp `overlap_seconds` to `[0,900]`. Generate the RSA-2048 key
   **before** the transaction (pure; reused on a `RunInTx` retry, discarded
   if the call turns out to be a replay).
3. One `RunInTx` (retried on `40001`/`40P01`; every value is re-read
   inside the closure): `LockForUpdate` → re-check revoked principal and
   the `rotation_id` replay under the lock → `FindActive` → if
   `ExpectActiveVersion` is set (scheduler only) and the active version
   differs, `optimistic_lock_conflict`, nothing written. Version =
   `MaxVersion + 1` over every row (revoked included), so it is never
   reused and concurrent callers get consecutive versions.
4. `secrets.Write` to OpenBao at `…/v{n}` — **before** any row write and
   the commit, bounded at 5s. A crash after the write but before the
   commit leaves material no row claims: the next attempt overwrites it
   (same `max+1`), otherwise the §8.6 reconciler reclaims it.
5. If a prior `active` row exists: end any other still-open overlap
   (`expires_at = now`, TS-INV-3 — at most one `rotating` version), then
   demote the prior `active` row to `rotating` (`expires_at =
   now+overlap`, cadence fields cleared) **before** inserting the new
   `active` row (`uq_sac_one_active` is checked per statement). `Insert`
   uses a `SAVEPOINT` so a `23505` can still be enriched with
   `active_rotation_id`.
6. Enqueue `ServiceAccountCredentialIssued` (no prior active) or
   `…Rotated` (`prior_version`, `expires_prior_at`) in the same tx; commit.
   Then a best-effort sweep of this principal's expired `rotating` rows
   runs on a detached 10s context (each through the TS-2 path). Response
   `201` with `Cache-Control: no-store`.
7. **Replay** (`replayIssueOrRotate`): a `revoked` row → `409
   credential_replay_revoked` (TS-D13 — never a misleading `502` from a
   missing OpenBao entry); past `ROTATION_REPLAY_WINDOW` (default 15m,
   `0` = unlimited) after `issued_at` → `409 credential_replay_expired`
   (TS-D22); otherwise re-read the key from OpenBao and return `200` with
   the same version. Every outcome is logged at Info (identifiers only)
   and counted in `credential_replays_total{result}` (TS-D23).
8. Under EXT-6 the caller discards the key and has RP-17 clear Keycloak's
   key cache so it re-fetches this service's JWKS — this service never
   writes Keycloak.

## 8.7 TS-2 — revoke (incl. break-glass, TS-INV-7)

1. `FindByVersion` (`404` if absent). Already `revoked` → idempotent `200`.
2. `RunInTx`: `LockForUpdate`, re-read the row under the lock (revoked
   meanwhile → idempotent `200`), then `secrets.Delete` (OpenBao KV v2
   metadata delete, bounded at 5s) **before** the commit — material-first
   in the destructive direction. Delete is a no-op on a missing path.
3. Same transaction: `Update` (guarded on `record_version`) → `revoked`,
   enqueue `ServiceAccountCredentialRevoked`.
4. **Concurrent-revoke race (TS-D13):** if the write still hits
   `optimistic_lock_conflict` (the sweep revokes without the lock), re-read;
   if now `revoked`, return that as success.
5. **TS-INV-7 (two halves):** this does not stop the key authenticating at
   Keycloak. Under EXT-6 there is no Keycloak-side TTL: the key stops
   working only after RP-17 clears Keycloak's key cache. The rotator and
   scheduler call RP-17 themselves; for TS-2 the caller must pair it with
   the Realm Provisioner action; offboarding relies on realm deletion.

## 8.3 Overlap-expiry sweep (`cmd/rotator/sweep.go`)

Each rotator run (every 5 min, `ROTATOR_RUN_TIMEOUT` 180s) does, in order:

0. **Owed refreshes first** (`retryPendingKeyRefreshes`, shared with the
   scheduler): list `keys_refresh_pending` rows that are committed or
   whose intent expired (reconciler pool, ≤ `ROTATOR_BATCH_LIMIT`), oldest
   first; RP-17 each, then `Clear` up to the `requested_at` observed. Uses
   at most half the run budget (each call needs 10s left). RP-17 failing
   for a tenant with no principal left (offboarded) drops the marker
   (`Dropped`); any other failure keeps it and counts `Failed`.
1. `ListExpiredRotating` (reconciler pool, `idx_sac_overlap`, ≤
   `ROTATOR_BATCH_LIMIT`, default 500). A listing failure is `Failed` —
   never a silent no-op.
2. Group by tenant. A tenant starts only if 30s of run budget remain (20s
   per row + 10s for RP-17), and each further row re-checks that; the rest
   are `Deferred` (next run). A tenant runs on a context detached from
   SIGTERM: a signal stops further tenants/rows but never cuts an
   in-flight revoke or the RP-17 for what committed.
3. Per row, one RLS-scoped app-pool transaction bound to the row's tenant:
   re-read (`rotating` no longer → `Skipped`), OpenBao delete, `Update` →
   `revoked` (an `optimistic_lock_conflict` here is `Skipped`, not
   `Failed`), `MarkPending` on `keys_refresh_pending`, enqueue
   `ServiceAccountCredentialRevoked` — all one commit.
4. After the tenant's rows: RP-17 (`RefreshKeys`, detached 10s context),
   then `Clear` the marker up to the value this tenant's revokes wrote. An
   RP-17 failure counts each of that tenant's revokes as `Failed`
   (`rotation_sweep_total{result="error"}` pages) — the rows are
   `revoked` and never re-enumerated, but the marker stays, so the next
   run's step 0 retries it (self-heals; TS-D22).

## 8.2b Automatic cadence rotation (`cmd/scheduler/scan.go`, §16 TSQ-6)

Same run shape as the rotator (`SCHEDULER_RUN_TIMEOUT` 180s, step 0
first, `SCHEDULER_BATCH_LIMIT` 500, tenant-by-tenant, 30s reserve, SIGTERM
finishes the current tenant):

1. `ListDueForRotation` (reconciler pool, `idx_sac_next_rotation`).
2. Per due row: `MarkIntent` (own transaction; `intent_until` ≈ the run's
   remaining time + 40s, at least 4 min) → `CredentialService.IssueOrRotate`
   with a fresh `rotation_id`, `ROTATION_DEFAULT_OVERLAP_SECONDS` (startup
   fails outside `[0,900]`) and `ExpectActiveVersion` = the scanned version
   → unless skipped, `MarkPending` (re-mark committed, so the intent can't
   be mistaken for nothing-owed and a concurrent sweep's `Clear` can't
   erase it).
3. Outcomes: `rotated`; `skipped` (`principal_revoked`,
   `principal_not_found`, `rotation_in_flight`, or
   `optimistic_lock_conflict` = rotated meanwhile); `failed` (anything
   else — possibly committed, so the marker is kept).
4. After the tenant: if anything rotated, RP-17 then `Clear`; on RP-17
   failure re-mark committed and count each rotation `failed`
   (`cadence_rotation_total{result="failed"}` pages). `next_rotation_at`
   has already advanced, but the marker makes the next run retry RP-17
   (TS-D22/23). If nothing rotated and this run created the marker, it is
   withdrawn.

## 8.6 Orphan-material reconciler (`cmd/rotator/orphan_reconciler.go`)

1. `ListPrincipalMaterialStates` (reconciler pool): every live principal
   with its credential rows (path, version, live flag). Failure = `Failed`.
2. Iterate from an offset derived from the run start time (wrapping), so
   an exhausted budget doesn't always starve the same principals; a
   principal starts only with 20s left, the rest are `Deferred`.
3. Per principal, `List` OpenBao under the current client-id prefix plus
   the parent of every stored `openbao_path` (a re-mint can leave older
   versions under the old prefix). Per version found:
   - live row → `ok`;
   - revoked row whose material survived → deleted (`orphan_deleted`);
   - same version recorded under another prefix → left for that prefix;
   - no row → **under the principal lock** in an RLS-scoped transaction,
     re-check that no row claims it, then delete (5s bound,
     `orphan_deleted`). Lock busy (`rotation_in_flight`) or principal gone
     → `Skipped`.
4. Per live row whose material was not found: unlocked
   `IsCredentialLive` pre-filter (revoked since → fine), then **under the
   principal lock** re-read the row and `List` its own prefix (not `Read`
   — no plaintext pulled, and missing vs outage stays distinguishable).
   Still live with no material → `missing_material` (page; resolved by
   rotation, never by fabricating material). Lock busy → `Skipped`.

## Retention prune (`cmd/rotator/prune.go`)

After the reconciler, each step only if 5s of budget remain:
`processed_events` (`tenant_offboarding`) older than
`PROCESSED_EVENTS_TTL_DAYS` (8) via `inbox.Store.Prune` (loops
`PRUNE_BATCH_LIMIT`-sized batches until done); published `outbox_events`
older than `OUTBOX_PRUNE_OLDER_THAN` (168h) via the outbox runner's
`PrunePublished`; `rls_violation_log` older than
`RLS_VIOLATION_LOG_TTL_DAYS` (30) via `prune_rls_violation_log()` in
batches (app pool — the app role has no direct table grant). A step cut
off by the deadline or a signal is `Deferred`, not `Failed`.

**Exit code:** the rotator exits 1 if the sweep (including step 0), the
reconciler (`Failed` or `missing_material`) or any prune failed; the
scheduler exits 1 on any `failed`. Deferred work alone exits 0. Both
serve `/metrics` for `*_METRICS_SCRAPE_GRACE` (15s) before exiting.

## 8.4 Offboarding cascade (`TenantMembershipsPurged`)

0. **Before `Handle`** (`cmd/consumer`): `GlueDecoder` strips the Glue
   header; the payload is validated against the embedded
   `tenant_memberships_purged.json`. A violation never reaches the steps
   below — counted (`consumed_schema_violations_total`), sent to the DLQ
   (`schema_violation`), acked.
1. Envelope id: a `TenantMembershipsPurged` with a missing or non-UUID id
   → error → the DLQ router dead-letters it (`invalid_envelope_id`,
   TS-D22) and acks. An unknown event type → `ackUnknown` (metric, info
   log, claimed in `processed_events`) — never retried, so a producer
   addition can't DLQ-storm the queue.
2. Decode the payload (`tenant_id` required) and bind the GUC to it + the
   system principal.
3. platform-events' inbox (`InboxRepository.ProcessOnce` →
   `inbox.Store.Process`) opens ONE app-pool transaction (`SET LOCAL
   app.tenant_id`) and claims the envelope id. Already claimed → duplicate:
   ack, no-op. A concurrent copy blocks on the claim's row lock, then sees
   the duplicate (or, past `PG_LOCK_TIMEOUT`, fails and is redelivered as a
   plain duplicate later).
4. In that transaction: `LockByTenant` (FOR UPDATE, so no TS-1/TS-2 for the
   tenant runs mid-cascade) → `ListByPrincipal` per principal to collect
   every credential's OpenBao path.
5. Delete every collected path, then walk the tenant's whole OpenBao
   subtree (`iam/serviceaccount/<tenant_id>`, depth ≤ 4) and delete
   anything left — all **before** the commit (§9.3/§15.2): once the
   principal rows are gone the §8.6 reconciler can never find this
   tenant's material again. Then `DeleteByTenant` (FK-cascades credentials)
   → enqueue `ServiceAccountRevoked` per principal → COMMIT (claim
   included). The OpenBao calls run with the transaction open, which is
   why `SQS_HANDLER_TIMEOUT` (45s) sits below the 60s visibility timeout.
6. **Partial failure:** any error rolls back the whole transaction,
   claim included; SQS redelivers and the cascade repeats (re-deletes are
   no-ops). If retries exhaust `maxReceiveCount` and the message DLQs, the
   Postgres rows remain while some material is gone — the §8.6 reconciler
   reports the missing entries as `missing_material` (page) rather than
   losing them silently.

## JWKS fetch (EXT-6)

Rate-limit check (per-tenant bucket, then the global bucket for a known
tenant or the unknown bucket otherwise; `429 rate_limited`) →
`JWKSService.PublicKeys` on the app pool with the path tenant as the GUC
(RLS applies) → the tenant's active `platform_automation` principal →
every `active`/`rotating`, non-expired credential → read the PEM from
OpenBao and derive the public JWK (`kid` = row id). Unreadable keys are
skipped and counted; the active key (or every key) unreadable → `503
jwks_keys_unavailable`. A tenant that got keys is marked known until the
next DB reload. The known-tenant set reloads every
`JWKS_KNOWN_TENANTS_REFRESH` (15s); a failed reload keeps the previous set.

## 9. Concurrency, idempotency, failure — summary table

| Failure | Handling |
|---|---|
| OpenBao unavailable on TS-1 write | `502 secret_store_unavailable`; nothing committed (material-first) |
| Crash after OpenBao write, before commit | Material no row claims — overwritten by the next attempt or reclaimed by §8.6 under the principal lock |
| Lost TS-1 response | Retry same `rotation_id` within the replay window → same version + key, no double-rotation |
| Replay past the window / of a revoked version | `409 credential_replay_expired` / `409 credential_replay_revoked` |
| Concurrent TS-1/TS-2/offboarding for one principal | Serialized on the principal row lock; `409 rotation_in_flight` only if the lock isn't free within `PG_LOCK_TIMEOUT` |
| Concurrent-revoke race (TS-2 vs sweep) | Re-checked, idempotent outcome returned / `Skipped`, never `409` or `Failed` |
| Issue against a revoked principal | `422 principal_revoked` |
| Postgres unavailable / statement timeout | `503 db_unavailable` (SQLSTATE class 08/53/57/58) |
| RP-17 fails after a sweep revoke or scheduled rotation | Counted `Failed` (pages); `keys_refresh_pending` marker retried at the start of the next run |
| CronJob killed mid-run | Marker already committed with the revoke (sweep) or as an intent (scheduler) — the next run retries RP-17 |
| Offboarding event redelivered or delivered concurrently | Inbox claim — the second copy waits on the claim's row lock, sees a duplicate, acks |
| Offboarding cascade fails midway | Whole transaction (claim included) rolls back; SQS redelivers |
| Offboarding payload fails its schema / bad envelope id | Straight to the DLQ (`schema_violation` / `invalid_envelope_id`) + ack, cascade not run, critical alert; normal redrive if the DLQ can't be resolved or the send fails |
| Outbox relay lag / SNS outage | Queues in `outbox_events`; drains on recovery; poison events move to `outbox_dead_letters` |

## 15. GDPR / data lifecycle

Tenant hard-delete flows entirely through the offboarding cascade above —
no separate soft-delete-then-purge path. A revoke's OpenBao delete
(CUST-2) is a full KV v2 metadata delete, not a recoverable one. Because
plaintext lives only in OpenBao, a Postgres restore never resurrects a
secret; a metadata/OpenBao divergence is reconciled by rotation, never by
reading a stale secret.
