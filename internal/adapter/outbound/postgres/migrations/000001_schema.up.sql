-- ─────────────────────────────────────────────────────────────────────────
-- Token Service — consolidated initial schema (LLD §4, §19). All names below
-- are frozen by LLD §25 and must match character-for-character.
--
-- outbox_events is created by platform-events' outbox.ApplySchema, which
-- cmd/server/main.go (and the test suite) runs BEFORE this migration
-- (§4.4/§19 MIG-2 ordering invariant) — this migration only customizes it
-- (payload column + trigger + grants, guarded IF EXISTS) and never defines
-- it (MIG-1). processed_events is this service's own DDL.
--
-- Dev-stage posture: nothing is deployed anywhere (§15/§19), so this is the
-- only migration this service has ever shipped and migrations are outright
-- (no expand/contract dance).
-- ─────────────────────────────────────────────────────────────────────────

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

-- §4.1 enums (frozen, §25)
CREATE TYPE principal_type    AS ENUM ('platform_automation');            -- MVP; 'tenant_bot','user_pat' added post-launch (§2.4)
CREATE TYPE principal_status  AS ENUM ('active', 'revoked');
CREATE TYPE credential_status AS ENUM ('active', 'rotating', 'revoked');

-- ─────────────────────────────────────────────────────────────────────────
-- app_tenant_id() — reads the tenant GUC set by pgcommon's GUC bridge
-- (SET LOCAL app.tenant_id, RLS-6). STABLE (evaluates once per statement),
-- SECURITY DEFINER (invoker cannot poison the search path), fail-closed
-- (returns NULL on any error so RLS blocks rather than silently opens).
-- §4.3.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION app_tenant_id() RETURNS uuid
LANGUAGE plpgsql STABLE SECURITY DEFINER AS $$
DECLARE v text;
BEGIN
  v := current_setting('app.tenant_id', true);
  IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
  RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- rls_check_tenant() — the policy predicate used identically in USING and
-- WITH CHECK on both tenant-scoped tables (§4.3).
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION rls_check_tenant(p_tenant_id uuid, p_table_name text)
RETURNS boolean
LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
SET search_path = public AS $$
DECLARE v_app uuid;
BEGIN
  v_app := app_tenant_id();
  IF v_app IS NULL     THEN RETURN false; END IF;   -- fail-closed: no GUC → no rows / write rejected
  IF p_tenant_id <> v_app THEN RETURN false; END IF;-- cross-tenant → hidden / rejected
  RETURN true;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- touch_row() — BEFORE UPDATE on every record_version-carrying table (§4.5).
-- Fires only when the row actually changed (WHEN (OLD.* IS DISTINCT FROM
-- NEW.*)), so an idempotent replay that produces an identical row does not
-- bump record_version or spuriously invalidate a concurrent optimistic-lock
-- holder.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION touch_row() RETURNS trigger AS $$
BEGIN
  NEW.updated_at     := now();
  NEW.record_version := OLD.record_version + 1;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ═════════════════════════════════════════════════════════════════════════
-- §4.2 service_account_principals
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE service_account_principals (
  id                  uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id           uuid NOT NULL,
  principal_sub       uuid NOT NULL,                 -- the Keycloak service-account client's user sub (from RP, TS-4)
  keycloak_client_id  text NOT NULL,                 -- always 'platform-automation' at MVP (frozen, §25)
  principal_type      principal_type NOT NULL DEFAULT 'platform_automation',
  status              principal_status NOT NULL DEFAULT 'active',
  record_version      integer NOT NULL DEFAULT 1 CHECK (record_version > 0),  -- optimistic-lock token; bumped by trigger (§4.5)
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  deleted_at          timestamptz,
  CONSTRAINT service_account_principals_pkey PRIMARY KEY (id),
  CONSTRAINT uq_sap_id_tenant               UNIQUE (id, tenant_id),          -- FK target for the composite child FK
  CONSTRAINT uq_sap_active_principal        UNIQUE (tenant_id, principal_type) -- one automation principal per tenant
);

CREATE INDEX idx_sap_tenant        ON service_account_principals (tenant_id);
CREATE INDEX idx_sap_sub           ON service_account_principals (tenant_id, principal_sub);   -- reconcile-by-sub (RP-18)
CREATE INDEX idx_sap_status        ON service_account_principals (tenant_id, status) WHERE deleted_at IS NULL;

CREATE TRIGGER service_account_principals_touch_row BEFORE UPDATE ON service_account_principals
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();

ALTER TABLE service_account_principals ENABLE ROW LEVEL SECURITY;
ALTER TABLE service_account_principals FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON service_account_principals FROM PUBLIC;
CREATE POLICY service_account_principals_rls ON service_account_principals
  USING      (rls_check_tenant(tenant_id, 'service_account_principals'))
  WITH CHECK (rls_check_tenant(tenant_id, 'service_account_principals'));

-- ═════════════════════════════════════════════════════════════════════════
-- §4.2 service_account_credentials
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE service_account_credentials (
  id             uuid NOT NULL DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL,
  principal_id   uuid NOT NULL,
  version        integer NOT NULL,                    -- monotonically increasing per principal (§6.2)
  status         credential_status NOT NULL DEFAULT 'active',
  openbao_path   text NOT NULL,                       -- deterministic KV v2 path; NOT the secret (§6.3, §10.5)
  granted_by     uuid NOT NULL,                        -- x-user-id of the actor (iam-system on cron/RP paths)
  rotation_id    uuid,                                -- the caller's idempotency key for the issue/rotate that created this row (§9.2); NULL for the offboarding-revoke path
  record_version integer NOT NULL DEFAULT 1 CHECK (record_version > 0),
  issued_at      timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),   -- last status transition (rotate/sweep/revoke); maintained by the trigger (§4.5)
  rotated_at     timestamptz,
  expires_at     timestamptz,                          -- nullable; rotation-overlap cutoff for a 'rotating' row (§6.2)
  revoked_at     timestamptz,
  deleted_at     timestamptz,
  CONSTRAINT service_account_credentials_pkey PRIMARY KEY (id),
  CONSTRAINT fk_sac_principal FOREIGN KEY (principal_id, tenant_id)
      REFERENCES service_account_principals (id, tenant_id) ON DELETE CASCADE,
  CONSTRAINT uq_sac_version   UNIQUE (principal_id, version)
);

CREATE INDEX idx_sac_tenant    ON service_account_credentials (tenant_id);
CREATE INDEX idx_sac_principal ON service_account_credentials (principal_id, version DESC);  -- TS-3 metadata read (§8.5)
CREATE INDEX idx_sac_overlap   ON service_account_credentials (expires_at)
                                  WHERE status = 'rotating' AND expires_at IS NOT NULL;       -- the §8.3 overlap-expiry sweep scans exactly this
CREATE UNIQUE INDEX uq_sac_one_active
  ON service_account_credentials (principal_id) WHERE status = 'active' AND deleted_at IS NULL;
CREATE UNIQUE INDEX uq_sac_rotation_id
  ON service_account_credentials (principal_id, rotation_id) WHERE rotation_id IS NOT NULL;   -- TS-1 idempotency (§9.2)

CREATE TRIGGER service_account_credentials_touch_row BEFORE UPDATE ON service_account_credentials
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();

ALTER TABLE service_account_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE service_account_credentials FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON service_account_credentials FROM PUBLIC;
CREATE POLICY service_account_credentials_rls ON service_account_credentials
  USING      (rls_check_tenant(tenant_id, 'service_account_credentials'))
  WITH CHECK (rls_check_tenant(tenant_id, 'service_account_credentials'));

-- ═════════════════════════════════════════════════════════════════════════
-- processed_events — service-owned DDL, the inbound offboarding-consumer
-- dedup ledger (§4.2, §7.1, §9.2). Operational and RLS-exempt (not a
-- tenant-query surface) — outbox_events, processed_events are NOT
-- tenant-scoped (§4.3).
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE processed_events (
  event_id     text NOT NULL,                     -- TEXT, not uuid: dedupes replayed SQS/SNS deliveries whose broker message IDs are external strings
  consumer     text NOT NULL,                     -- 'tenant_offboarding'
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (event_id, consumer)                -- composite; the table's sole job is idempotency (§7.6)
);
CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);  -- prunes the retention sweep's range scan

-- ═════════════════════════════════════════════════════════════════════════
-- outbox_events customization (MIG-1/MIG-2, §4.2/§4.4/§19). platform-events'
-- outbox.ApplySchema creates outbox_events with a JSONB payload column
-- BEFORE this migration runs; pgx encodes []byte as bytea hex in PgBouncer
-- SimpleProtocol mode, which is invalid for jsonb — text accepts the raw
-- bytes as-is, and the outbox runner JSON-unmarshals it back either way
-- (§7.3.1). Every GRANT/ALTER against outbox_events is IF EXISTS-guarded
-- since this domain migration never defines that table's schema (MIG-1).
--
-- FOOTGUN if this "runs once, against an empty DB, before any traffic"
-- invariant is ever violated (§4.4 — this is the only consolidated
-- migration this service has ever shipped): a USING-clause ALTER COLUMN
-- TYPE takes an ACCESS EXCLUSIVE lock and rewrites the entire table. On the
-- intended path that's instant (the table is empty). Do not reuse this file
-- as a template for re-running against a populated outbox_events.
-- ═════════════════════════════════════════════════════════════════════════
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
    ALTER TABLE outbox_events ALTER COLUMN payload TYPE text USING payload::text;
  END IF;
END$$;

-- pgx SimpleProtocol encodes []byte as bytea hex (\x...) even for text
-- columns. This trigger decodes it back to UTF-8 text on every INSERT so
-- the outbox runner can JSON-unmarshal the payload without errors.
CREATE OR REPLACE FUNCTION outbox_normalize_payload()
RETURNS trigger AS $$
BEGIN
  IF left(NEW.payload, 2) = '\x' THEN
    NEW.payload = convert_from(decode(substring(NEW.payload FROM 3), 'hex'), 'UTF8');
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
    DROP TRIGGER IF EXISTS trg_outbox_normalize_payload ON outbox_events;
    CREATE TRIGGER trg_outbox_normalize_payload
      BEFORE INSERT ON outbox_events
      FOR EACH ROW EXECUTE FUNCTION outbox_normalize_payload();
  END IF;
END$$;

-- ═════════════════════════════════════════════════════════════════════════
-- Roles & grants (§4.3, §4.4, RLS-1..RLS-7). Four DB roles in play:
--   • serviceaccount_app          — runtime app (cmd/server, cmd/consumer).
--                                    Does NOT hold BYPASSRLS.
--   • serviceaccount_migrator     — runs this migration. Holds BYPASSRLS
--                                    explicitly and narrowly (so DDL/data
--                                    fixes are never blocked by FORCE RLS
--                                    with no GUC set, §4.4).
--   • serviceaccount_reconciler   — cmd/rotator's cross-tenant enumeration
--                                    role (§4.3/RLS-7). NOLOGIN BYPASSRLS,
--                                    SELECT-only on the two service_account_*
--                                    tables, no INSERT/UPDATE/DELETE grant —
--                                    every write the rotator performs goes
--                                    through a normal RLS-scoped
--                                    serviceaccount_app RunInTx instead
--                                    (RLS-7: BYPASSRLS is read-only and
--                                    enumeration-only).
--   • admin_readonly               — cross-tenant human operator/support
--                                    SELECTs, kept structurally separate from
--                                    serviceaccount_reconciler (§4.3): the
--                                    service's own jobs never use
--                                    admin_readonly, and the API path never
--                                    uses either.
--
-- Roles are typically provisioned by infra (Terraform / the test harness)
-- ahead of this migration, matching the sibling Realm Provisioner service;
-- this migration idempotently attaches grants and re-asserts
-- BYPASSRLS/NOBYPASSRLS, skipping gracefully (NOTICE, not ERROR) when a role
-- does not yet exist or the current user lacks privilege to alter it, so the
-- same file is safe to run in every environment (dev/test/CI/prod).
-- ═════════════════════════════════════════════════════════════════════════
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_migrator') THEN
    BEGIN
      ALTER ROLE serviceaccount_migrator BYPASSRLS;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'serviceaccount_migrator exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh (fine in dev).';
    END;
  END IF;

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app' AND rolbypassrls) THEN
    BEGIN
      ALTER ROLE serviceaccount_app NOBYPASSRLS;
      RAISE NOTICE 'Stripped BYPASSRLS from serviceaccount_app (RLS invariant restored).';
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE WARNING 'serviceaccount_app has BYPASSRLS but current user cannot ALTER ROLE — VIOLATION requires operator action.';
    END;
  END IF;

  -- serviceaccount_reconciler: enumeration-only BYPASSRLS role (RLS-7). Must
  -- hold BYPASSRLS (to enumerate cross-tenant) but never NOBYPASSRLS.
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_reconciler') THEN
    BEGIN
      ALTER ROLE serviceaccount_reconciler BYPASSRLS;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'serviceaccount_reconciler exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh (fine in dev).';
    END;
  END IF;

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
    BEGIN
      ALTER ROLE admin_readonly BYPASSRLS;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'admin_readonly exists but current user cannot ALTER ROLE — skipping BYPASSRLS refresh (fine in dev).';
    END;
  END IF;
END$$;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app') THEN
    RAISE NOTICE 'serviceaccount_app role missing — skipping grants. Infra provisions this role ahead of the migration in prod.';
  ELSE
    BEGIN
      GRANT EXECUTE ON FUNCTION app_tenant_id()              TO serviceaccount_app;
      GRANT EXECUTE ON FUNCTION rls_check_tenant(uuid, text) TO serviceaccount_app;

      GRANT SELECT, INSERT, UPDATE, DELETE ON service_account_principals  TO serviceaccount_app;
      GRANT SELECT, INSERT, UPDATE, DELETE ON service_account_credentials TO serviceaccount_app;
      GRANT SELECT, INSERT, DELETE         ON processed_events            TO serviceaccount_app;

      -- outbox_events: created by platform-events; guarded IF EXISTS because
      -- this domain migration only customizes it, never defines it (MIG-1).
      IF EXISTS (SELECT 1 FROM information_schema.tables
                 WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_events TO serviceaccount_app;
      END IF;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to serviceaccount_app skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
END$$;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_reconciler') THEN
    RAISE NOTICE 'serviceaccount_reconciler role missing — skipping grants. Infra provisions this role ahead of the migration in prod.';
  ELSE
    BEGIN
      -- SELECT-only — no INSERT/UPDATE/DELETE grant. RLS-7: this role may
      -- enumerate across tenants (BYPASSRLS) but never writes; every write
      -- the cmd/rotator crons perform goes through a normal RLS-scoped
      -- serviceaccount_app RunInTx bound to the row's own tenant.
      GRANT SELECT ON service_account_principals  TO serviceaccount_reconciler;
      GRANT SELECT ON service_account_credentials TO serviceaccount_reconciler;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to serviceaccount_reconciler skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
END$$;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
    RAISE NOTICE 'admin_readonly role missing — skipping grants. Infra provisions this role ahead of the migration in prod.';
  ELSE
    BEGIN
      GRANT SELECT ON service_account_principals  TO admin_readonly;
      GRANT SELECT ON service_account_credentials TO admin_readonly;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
END$$;
