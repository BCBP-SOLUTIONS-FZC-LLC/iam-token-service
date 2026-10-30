---
name: new-migration
description: Scaffold a new numbered DB migration with correct RLS, grants, and search_path patterns for iam-token-service
disable-model-invocation: true
---

Create a new migration pair at:
  `deploy/iam/migrations/000NNN_<name>.up.sql`
  `deploy/iam/migrations/000NNN_<name>.down.sql`

where `NNN` is the next sequence number after the highest existing file in that directory.

---

## Before you start

Check what number comes next:
```bash
ls deploy/iam/migrations/*.sql | sort | tail -4
```

Ask the user:
- **What does this migration do?** (add table / add column / add index / add function / drop something)
- **Is the new table tenant-scoped** (needs RLS) **or RLS-exempt** (like `keys_refresh_pending`)?
- **Which roles need which access?** (default: `app_user` = SELECT/INSERT/UPDATE/DELETE; `app_health` = SELECT; `app_reconciler` = BYPASSRLS via role, no explicit GRANT needed unless the table is RLS-exempt)

---

## Template: new tenant-scoped table

```sql
-- 000NNN_<name>.up.sql
-- <one-line description>

CREATE TABLE <name> (
    id          UUID        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- add columns here
    CONSTRAINT <name>_pkey PRIMARY KEY (id)
);

ALTER TABLE <name> ENABLE ROW LEVEL SECURITY;
ALTER TABLE <name> FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON <name>
    USING (tenant_id = app_tenant_id());

GRANT SELECT, INSERT, UPDATE, DELETE ON <name> TO app_user;
GRANT SELECT                          ON <name> TO app_health;
REVOKE ALL                            ON <name> FROM PUBLIC;
```

```sql
-- 000NNN_<name>.down.sql
DROP TABLE IF EXISTS <name>;
```

---

## Template: RLS-exempt table (e.g. cross-tenant bookkeeping)

```sql
-- 000NNN_<name>.up.sql
-- <one-line description>

CREATE TABLE <name> (
    id         UUID        NOT NULL DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- add columns here
    CONSTRAINT <name>_pkey PRIMARY KEY (id)
);

-- No RLS — reconciler and app_user both need cross-tenant access.
GRANT SELECT, INSERT, UPDATE, DELETE ON <name> TO app_user;
GRANT SELECT, INSERT, UPDATE, DELETE ON <name> TO app_reconciler;
GRANT SELECT                          ON <name> TO app_health;
REVOKE ALL                            ON <name> FROM PUBLIC;
```

---

## Rules enforced in CI

1. **No bare `SET app.tenant_id`** — always `SET LOCAL` (RLS-6).
2. **FORCE ROW LEVEL SECURITY** — must appear on every tenant-scoped table.
3. **REVOKE … FROM PUBLIC** — all new tables must revoke the default PUBLIC grant.
4. **`app_tenant_id()` function** — use this (not a raw column reference) in RLS policies; it reads `current_setting('app.tenant_id')::uuid` with a pinned `search_path`.
5. **Functions** — any new SQL function must set `search_path = ''` and use fully-qualified names to avoid search-path injection.

## Validation

After creating:
```bash
make schema-validate   # checks schema governance
make docker-up         # start local infra
# apply migrations:
DATABASE_URL=... goose -dir deploy/iam/migrations postgres up
```
