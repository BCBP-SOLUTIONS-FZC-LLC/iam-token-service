# Database schema

Summarized from `internal/adapter/outbound/postgres/migrations/` (five
migrations, dev stage, nothing deployed). Read those files for exact DDL;
this is "what the schema means," not the literal SQL.

## Extensions and enums

- `pgcrypto` (for `gen_random_uuid()`).
- `principal_type` — `platform_automation` (MVP only; post-launch adds
  `tenant_bot`, `user_pat` additively, §2.4 of the LLD).
- `principal_status` — `active`, `revoked`.
- `credential_status` — `active`, `rotating`, `revoked`.

## Tables

### `service_account_principals` (tenant-scoped, RLS)

One row per tenant automation principal. Columns: `id`, `tenant_id`,
`principal_sub` (the Keycloak service-account client's user sub, from RP),
`keycloak_client_id` (`platform-automation` or
`platform-automation-<tenant_id>`, validated by
`domain.ValidPlatformAutomationClientID`), `principal_type`, `status`,
`record_version` (optimistic-lock token, `CHECK (record_version > 0)`),
`created_at`, `updated_at`, `deleted_at`.

- `uq_sap_id_tenant UNIQUE (id, tenant_id)` — FK target for the composite
  child FK below.
- `uq_sap_active_principal UNIQUE (tenant_id, principal_type)` — one
  automation principal per tenant; TS-4's `INSERT … ON CONFLICT … DO
  UPDATE` (only when the sub or client id changed) relies on it.
- `idx_sap_tenant (tenant_id)`, `idx_sap_sub (tenant_id, principal_sub)`
  (find-by-sub, RP-18), `idx_sap_status (tenant_id, status) WHERE
  deleted_at IS NULL`.
- The row is also the **principal lock**: TS-1, TS-2 and the orphan
  reconciler take `SELECT … FOR UPDATE` on it (`LockForUpdate`), the
  offboarding cascade locks all of a tenant's rows (`LockByTenant`). A
  wait past `PG_LOCK_TIMEOUT` (`55P03`) maps to `409 rotation_in_flight`.

### `service_account_credentials` (tenant-scoped, RLS)

One row per issued/rotated credential version. Columns: `id` (also the
JWK `kid`), `tenant_id`, `principal_id`, `version` (`MaxVersion + 1` over
every row, never reused), `status`, `openbao_path` (the KV v2 path —
**never the secret itself**), `granted_by` (the actor), `rotation_id`
(the caller's idempotency key), `record_version`, `issued_at`,
`updated_at`, `rotated_at`, `expires_at` (the overlap cutoff for a
`rotating` row), `revoked_at`, `deleted_at`, and
`rotation_cadence_days`/`next_rotation_at` (`000002`, §16 TSQ-6 Resolved;
set on the `active` row at issue/rotate, default cadence 90 days, cleared
to `NULL` when the row is demoted — only the current `active` version is
ever "due").

- `fk_sac_principal FOREIGN KEY (principal_id, tenant_id) REFERENCES service_account_principals (id, tenant_id) ON DELETE CASCADE` —
  makes a cross-tenant credential row structurally impossible (Layer 3 of
  tenant isolation) and lets the offboarding cascade's `DeleteByTenant` on
  principals remove credentials too.
- `uq_sac_version UNIQUE (principal_id, version)`.
- `uq_sac_one_active UNIQUE (principal_id) WHERE status = 'active' AND deleted_at IS NULL` —
  **CONC-2**. It is a plain, immediately-checked partial unique index, so
  TS-1 demotes the prior `active` row to `rotating` *before* inserting the
  new one.
- `uq_sac_rotation_id UNIQUE (principal_id, rotation_id) WHERE rotation_id IS NOT NULL` —
  TS-1 idempotency (§9.2). `Insert` wraps the INSERT in a `SAVEPOINT` so
  that after a `23505` the `active_rotation_id` lookup can still run.
- `idx_sac_overlap (expires_at) WHERE status = 'rotating' AND expires_at IS NOT NULL` —
  the overlap-expiry sweep's scan.
- `idx_sac_next_rotation (next_rotation_at) WHERE status = 'active' AND next_rotation_at IS NOT NULL AND deleted_at IS NULL` —
  `cmd/scheduler`'s due-list scan.
- `idx_sac_principal (principal_id, version DESC)` — TS-3's metadata read;
  `idx_sac_tenant (tenant_id)`.

### `processed_events` (RLS-exempt, operational)

The offboarding-consumer dedup ledger, written and pruned only through
platform-events' `pkg/inbox` (`InboxRepository`, TS-D19): the claim is
the first statement of the cascade's own transaction on the RLS-scoped
app pool. `event_id` (`text` — broker ids are external strings),
`consumer` (`'tenant_offboarding'`, the only value), `processed_at`;
`PRIMARY KEY (event_id, consumer)` — a concurrent copy blocks on the
claim's row lock. `processed_events_processed_at_idx` supports the
retention prune (`inbox.Store.Prune` from `cmd/rotator`,
`PROCESSED_EVENTS_TTL_DAYS`, default 8, must exceed the SQS message
lifetime).

### `rls_violation_log` (RLS-exempt, operational, `000003`)

`id bigserial`, `table_name`, `row_tenant_id`, `app_tenant_id`,
`violation_type` (`missing_or_invalid_guc` | `cross_tenant_access`),
`session_role`, `application_name`, `query_text` (≤ 2048 chars),
`occurred_at` (indexed). RLS disabled (the logger runs inside policy
checks, so a policy here would recurse). No grant to `serviceaccount_app`:
rows are written only through `log_rls_violation()` and pruned only
through `prune_rls_violation_log(ttl_days, limit)` (cmd/rotator,
`RLS_VIOLATION_LOG_TTL_DAYS`, default 30; the function refuses
`ttl_days < 7` since `000004`). `serviceaccount_reconciler` and
`admin_readonly` have `SELECT` — cmd/server's exporter reads it into
`iam_rls_violations_total`. A cross-tenant write rejected by `WITH CHECK`
is never logged: its error rolls the log row back with the caller's
transaction.

### `keys_refresh_pending` (RLS-exempt, operational, `000005`)

The durable "RP-17 key-cache refresh owed for this tenant" marker (EXT-6,
TS-INV-7, TS-D22/23). Columns: `tenant_id uuid PRIMARY KEY`,
`requested_at timestamptz NOT NULL`, `intent_until timestamptz NULL`;
index on `requested_at` (the retry pass reads oldest first).

- `intent_until` **NULL = committed marker**: the revoke/rotation it
  covers has committed, so the next run's retry pass refreshes it at once.
  Written by the rotator sweep in the same transaction as each revoke
  (`MarkPending`), and by the scheduler after `IssueOrRotate` and after a
  failed RP-17.
- **Non-NULL = the scheduler's intent marker**, written in its own
  transaction *before* `IssueOrRotate` (`MarkIntent`); the retry pass
  ignores it until `intent_until` passes. A committed write always resets
  it to NULL; an intent write never downgrades a committed marker (an
  expired intent becomes committed).
- `requested_at` only moves forward (`GREATEST`). `Clear` deletes the row
  only while `requested_at <=` the value the caller observed, so a newer
  request is never erased.
- Grants: `serviceaccount_app` SELECT/INSERT/UPDATE/DELETE (marker writes
  and clears); `serviceaccount_reconciler` SELECT (the retry pass's
  `ListPending` and the server's `PendingStats` gauges). `REVOKE ALL FROM
  PUBLIC`.

### `outbox_events` / `outbox_dead_letters` (owned by `platform-events`)

Created by `outbox.ApplySchema`, which `Migrate` runs **before** this
service's migrations (MIG-1/MIG-2). `000001` only customizes
`outbox_events` (`payload` → `text`, `trg_outbox_normalize_payload`),
guarded `IF EXISTS`. `000004` grants `serviceaccount_app` CRUD on
`outbox_dead_letters` and `USAGE, SELECT` on `outbox_events_ordering_seq`,
so the runner can dead-letter a poison event.

## Row-Level Security

Both tenant-scoped tables: `ENABLE` + `FORCE ROW LEVEL SECURITY` (applies
even to the table owner) + `REVOKE ALL … FROM PUBLIC`, with one policy
used for both `USING` and `WITH CHECK`:

```sql
rls_check_tenant(tenant_id, table_name) →          -- STABLE STRICT SECURITY DEFINER
  v_app := app_tenant_id()                          -- current_setting('app.tenant_id', true)
  IF v_app IS NULL THEN log_rls_violation(…, 'missing_or_invalid_guc'); RETURN false END IF  -- fail-closed
  IF tenant_id <> v_app THEN log_rls_violation(…, 'cross_tenant_access'); RETURN false END IF
  RETURN true
```

`app_tenant_id()` is `STABLE SECURITY DEFINER` and fail-closed
(`EXCEPTION WHEN OTHERS THEN RETURN NULL`); since `000004` its
`search_path` is pinned (`pg_catalog, public`) and `EXECUTE` is revoked
from PUBLIC (granted to `serviceaccount_app`). The GUC is bound via
`SET LOCAL` (never plain `SET` — RLS-6; CI gate `set-local-only`), so a
pooled connection returned to PgBouncer carries no tenant binding.

`log_rls_violation()` (`SECURITY DEFINER`, swallows its own errors)
samples failures (1%, or `app.rls_violation_sample_rate`) into
`rls_violation_log`.

## Functions and triggers

| Name | Kind | Purpose |
|---|---|---|
| `app_tenant_id()` | function | Reads the tenant GUC, fail-closed |
| `rls_check_tenant(uuid, text)` | function | The RLS predicate on both tenant tables; logs failures (`000003`) |
| `log_rls_violation(text, uuid, text)` | function | Sampled insert into `rls_violation_log` |
| `prune_rls_violation_log(int, int)` | function | Batched delete; `ttl_days ≥ 7`, `limit ≥ 1` (`000004`); EXECUTE granted to `serviceaccount_app` |
| `touch_row()` | trigger fn | `BEFORE UPDATE … WHEN (OLD.* IS DISTINCT FROM NEW.*)` on both tenant tables: bumps `record_version` and `updated_at`. The `WHEN` guard means an identical-row update never invalidates a concurrent optimistic-lock holder |
| `outbox_normalize_payload()` | trigger fn | `BEFORE INSERT` on `outbox_events` |

## Roles and grants

| Role | Attributes | Grants | Used by |
|---|---|---|---|
| `serviceaccount_app` | `NOBYPASSRLS` (stripped by `000001` if present) | CRUD on both tenant tables, `outbox_events`, `outbox_dead_letters`, `keys_refresh_pending`; `SELECT, INSERT, DELETE` on `processed_events`; EXECUTE on `app_tenant_id`, `rls_check_tenant`, `prune_rls_violation_log` | `cmd/server`, `cmd/consumer`, and every write the CronJobs make (RLS-7) |
| `serviceaccount_migrator` | `BYPASSRLS` + DDL | table owner | the migrate Job, via `MIGRATION_DATABASE_URL` (direct port, bypasses PgBouncer for `pg_advisory_lock`) |
| `serviceaccount_reconciler` | `BYPASSRLS`, SELECT-only | `SELECT` on both tenant tables, `rls_violation_log`, `keys_refresh_pending` | the reconciler pool: `cmd/rotator`/`cmd/scheduler` enumeration, `cmd/server`'s exporters and JWKS known-tenant refresh |
| `admin_readonly` | `BYPASSRLS` | `SELECT` on both tenant tables and `rls_violation_log` (guarded: infra-provisioned, may not exist in dev/CI) | humans only, never the service |

Every grant block is guarded (`IF EXISTS … pg_roles`, `EXCEPTION WHEN
insufficient_privilege`), so migrations run in a dev database without the
roles.

## Migrations

| Migration | Adds | Down |
|---|---|---|
| `000001_schema` | Enums, both tenant tables, RLS, `processed_events`, outbox customization, roles/grants, `touch_row` | Guarded `REVOKE … FROM admin_readonly` (`EXCEPTION WHEN undefined_object`) so rollback works where the role doesn't exist |
| `000002_rotation_cadence` | `rotation_cadence_days`, `next_rotation_at`, `idx_sac_next_rotation` (additive; old rows stay `NULL` and are never due) | Drops them |
| `000003_rls_violation_log` | `rls_violation_log`, `log_rls_violation()`, `prune_rls_violation_log()`, the logging `rls_check_tenant()` | Restores `000001`'s non-logging predicate |
| `000004_hardening` (TS-D21) | Outbox dead-letter + ordering-sequence grants; `app_tenant_id()` search_path pinned, PUBLIC revoked; prune TTL ≥ 7 | Restores the previous function bodies, revokes the outbox grants |
| `000005_keys_refresh_pending` (TS-D22/23) | `keys_refresh_pending` + grants | Drops the table — trigger RP-17 by hand for any tenant still listed first |

`Migrate` (`postgres/migrate.go`) runs `outbox.ApplySchema` then these.
Helm runs them once per install/upgrade in a hook Job (`MIGRATE_ONLY=true`
on the server image); workloads set `RUN_MIGRATIONS=false`.

## Invariants

| # | Invariant |
|---|---|
| RLS-6 | `app.tenant_id` bound only via transaction-local `SET LOCAL`, never a session-wide `SET` |
| RLS-7 | Only `serviceaccount_reconciler` (the reconciler pools) may bypass RLS, and only for `SELECT`; every resulting write goes through an RLS-scoped `serviceaccount_app` transaction |
| CONC-1 | Optimistic locking on `record_version` for both tenant-scoped tables, plus the principal-row lock for every credential write (TS-D21) |
| CONC-2 | At most one `active` credential per principal at all times |
| CONC-3 | An emitted event always corresponds to a committed state change |

## Data model entities NOT owned here

- Keycloak client existence and key cache — Realm Provisioner.
- Membership/role grants for the automation principal — none exist by
  design (O&M AUTH-9).
- Audit log entries — Audit Log Service, consumed from this service's
  outbox events.
