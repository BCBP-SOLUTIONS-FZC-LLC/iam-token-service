# Database schema

Summarized from `internal/adapter/outbound/postgres/migrations/000001_schema.up.sql`
and `000002_rotation_cadence.up.sql` (two migrations so far — dev stage,
nothing deployed). Read those files directly for exact DDL; this is "what
the schema means," not the literal SQL.

## Extensions and enums

- `pgcrypto` (for `gen_random_uuid()`).
- `principal_type` — `platform_automation` (MVP only; post-launch adds
  `tenant_bot`, `user_pat` additively, §2.4 of the LLD).
- `principal_status` — `active`, `revoked`.
- `credential_status` — `active`, `rotating`, `revoked`.

## Tables

### `service_account_principals`

One row per tenant automation principal. Columns: `id`, `tenant_id`,
`principal_sub` (the Keycloak service-account client's user sub, from RP),
`keycloak_client_id` (always `platform-automation` at MVP),
`principal_type`, `status`, `record_version` (optimistic-lock token,
`CHECK (record_version > 0)`), `created_at`, `updated_at`, `deleted_at`.

- `uq_sap_id_tenant UNIQUE (id, tenant_id)` — FK target for the composite
  child FK below.
- `uq_sap_active_principal UNIQUE (tenant_id, principal_type)` — one
  automation principal per tenant; this is what makes TS-4 idempotent.
- `idx_sap_sub (tenant_id, principal_sub)` — reconcile-by-sub (RP-18).
- `idx_sap_status (tenant_id, status) WHERE deleted_at IS NULL`.

### `service_account_credentials`

One row per issued/rotated credential version. Columns: `id`, `tenant_id`,
`principal_id`, `version` (monotonically increasing per principal, never
reused), `status`, `openbao_path` (the KV v2 path — **never the secret
itself**), `granted_by` (the actor), `rotation_id` (nullable — the
caller's idempotency key; null for the offboarding-revoke path),
`record_version`, `issued_at`, `updated_at`, `rotated_at`, `expires_at`
(nullable — the rotation-overlap cutoff for a `rotating` row),
`revoked_at`, `deleted_at`, `rotation_cadence_days`/`next_rotation_at`
(`000002_rotation_cadence` — §16 TSQ-6 Resolved; set on the `active` row
at issue/rotate time, default cadence 90 days, cleared back to `NULL` the
instant that row is demoted to `rotating`/`revoked` — only the current
`active` version is ever "due").

- `fk_sac_principal FOREIGN KEY (principal_id, tenant_id) REFERENCES service_account_principals (id, tenant_id) ON DELETE CASCADE` —
  the composite FK that makes a cross-tenant credential row structurally
  impossible (Layer 3 of tenant isolation), and what makes the offboarding
  cascade's `DeleteByTenant` on principals cascade credentials for free.
- `uq_sac_version UNIQUE (principal_id, version)`.
- `uq_sac_one_active UNIQUE (principal_id) WHERE status = 'active' AND deleted_at IS NULL` —
  **CONC-2**, at most one `active` credential per principal, checked even
  under concurrent rotation. This is why TS-1's write ordering demotes the
  prior `active` row to `rotating` *before* inserting the new one — an
  insert-then-demote ordering would have both rows `active`
  simultaneously and violate this index immediately (it's a plain,
  non-deferrable partial unique index).
- `uq_sac_rotation_id UNIQUE (principal_id, rotation_id) WHERE rotation_id IS NOT NULL` —
  TS-1 idempotency (§9.2); a real concurrent-rotation collision here is
  what the `SAVEPOINT`/`ROLLBACK TO SAVEPOINT` in `credential_repository.go`'s
  `Insert` isolates so the `active_rotation_id` enrichment lookup can still
  run afterward.
- `idx_sac_overlap (expires_at) WHERE status = 'rotating' AND expires_at IS NOT NULL` —
  the exact partial index the §8.3 overlap-expiry sweep scans.
- `idx_sac_next_rotation (next_rotation_at) WHERE status = 'active' AND next_rotation_at IS NOT NULL AND deleted_at IS NULL` —
  `cmd/scheduler`'s due-list scan (§16 TSQ-6 Resolved).
- `idx_sac_principal (principal_id, version DESC)` — TS-3's metadata read.

### `processed_events` (RLS-exempt, operational)

The offboarding-consumer dedup ledger. `event_id` (`text`, not `uuid` —
dedupes replayed SQS/SNS deliveries whose broker message IDs are external
strings), `consumer` (`'tenant_offboarding'` today, the only value),
`processed_at`. `PRIMARY KEY (event_id, consumer)` — the table's sole job
is idempotency; `INSERT ... ON CONFLICT DO NOTHING` serializes duplicate
deliveries at the DB level. `processed_events_processed_at_idx` supports
the retention-prune sweep's range scan (`PROCESSED_EVENTS_TTL_DAYS`, must
exceed the SQS message lifetime).

### `outbox_events` (owned by `platform-events`, customized here)

Created by `outbox.ApplySchema` (not this migration — MIG-1/MIG-2
ordering: `outbox.ApplySchema` must run **before** this migration, since
this migration only customizes the `payload` column type + a normalize
trigger, guarded `IF EXISTS`, and never defines the table itself).

## Row-Level Security

Both tenant-scoped tables: `ENABLE ROW LEVEL SECURITY` + `FORCE ROW LEVEL SECURITY`
(applies even to the table owner) + `REVOKE ALL ... FROM PUBLIC`, with a
single policy applied to both `USING` and `WITH CHECK`:

```sql
rls_check_tenant(tenant_id, table_name) →
  v_app := app_tenant_id()          -- reads current_setting('app.tenant_id', true)
  IF v_app IS NULL THEN RETURN false END IF   -- fail-closed: no GUC → no rows / write rejected
  IF tenant_id <> v_app THEN RETURN false END IF  -- cross-tenant → hidden / rejected
  RETURN true
```

`app_tenant_id()` is `STABLE SECURITY DEFINER`, fail-closed (`EXCEPTION
WHEN OTHERS THEN RETURN NULL`) — any error classifying the GUC blocks
access rather than silently granting it. The GUC is bound via `SET LOCAL`
(never plain `SET` — RLS-6), transaction-local only, so a pooled
connection returned to PgBouncer carries no residual tenant binding into
the next checkout.

## Triggers

`touch_row()` — `BEFORE UPDATE`, fires only `WHEN (OLD.* IS DISTINCT FROM NEW.*)`,
bumps `record_version` and `updated_at`. The `WHEN` guard means an
idempotent replay that produces an identical row never spuriously
invalidates a concurrent optimistic-lock holder.

## Migrations

Two migrations so far. `000001_schema` is the consolidated base (tables,
RLS, roles, triggers). `000002_rotation_cadence` (§16 TSQ-6 Resolved)
adds `rotation_cadence_days`/`next_rotation_at` to
`service_account_credentials` plus `idx_sac_next_rotation` — additive
only, no data migration needed (pre-existing rows simply have both
columns `NULL`, which the due-list scan's own `WHERE` clause already
excludes). `000001`'s down migration's `REVOKE ... FROM admin_readonly`
is guarded (`EXCEPTION WHEN undefined_object`) to match its own up
migration's `IF NOT EXISTS (SELECT 1 FROM pg_roles ...)` guard on the
`GRANT` — `admin_readonly` is infra-provisioned ahead of the migration in
prod and may not exist in dev/CI/a fresh environment; an unguarded
`REVOKE` against a nonexistent role raises `role does not exist` and
aborts the whole rollback (verified against a real Postgres instance in
both branches).

## Roles

| Role | Attributes | Used by |
|---|---|---|
| `serviceaccount_app` | `NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE` | `cmd/server`, `cmd/consumer`; also `cmd/rotator`'s and `cmd/scheduler`'s own per-tenant RLS-scoped writes (RLS-7) |
| `serviceaccount_migrator` | `BYPASSRLS` + DDL | migrations only, direct connection |
| `serviceaccount_reconciler` | `BYPASSRLS`, `SELECT`-only | `cmd/rotator`'s and `cmd/scheduler`'s cross-tenant enumeration only (§16 TSQ-6 Resolved) |

## Invariants

| # | Invariant |
|---|---|
| RLS-6 | `app.tenant_id` bound only via transaction-local `SET LOCAL`, never a session-wide `SET` |
| RLS-7 | Only the `serviceaccount_reconciler` role (`cmd/rotator`'s and `cmd/scheduler`'s reconciler pools, §16 TSQ-6 Resolved) may bypass RLS, and only for `SELECT`-only cross-tenant enumeration — every resulting write goes through an RLS-scoped `serviceaccount_app` transaction |
| CONC-1 | Optimistic locking on `record_version` for both tenant-scoped tables |
| CONC-2 | At most one `active` credential per principal at all times, even under concurrent rotation |
| CONC-3 | An emitted event always corresponds to a committed state change — no committed-partial state a consumer can observe |

## Data model entities NOT owned here

- Keycloak client existence/secret value — Realm Provisioner.
- Membership/role grants for the automation principal — none exist by
  design (O&M AUTH-9); this service issues credentials only.
- Audit log entries — Audit Log Service, consumed from this service's
  outbox events, not queried directly.
