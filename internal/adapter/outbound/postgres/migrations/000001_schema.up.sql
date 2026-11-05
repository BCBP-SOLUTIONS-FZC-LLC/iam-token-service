-- ─────────────────────────────────────────────────────────────────────────
-- Token Service — consolidated schema (LLD §4, §19). All names are frozen
-- by LLD §25 and must match character-for-character.
--
-- outbox_events is created by platform-events' outbox.ApplySchema, which
-- cmd/server/main.go (and the test suite) runs BEFORE this migration
-- (§4.4/§19 MIG-2 ordering invariant). This migration only customizes it
-- (payload column type + normalize trigger + grants, all IF EXISTS guarded)
-- and never defines it (MIG-1). processed_events is this service's own DDL.
--
-- Dev-stage posture: nothing deployed anywhere (§15/§19). All content that
-- was previously spread across five numbered migration files (000001–000005)
-- is collapsed into this single file so the schema can be applied from
-- scratch in one shot. Function bodies reflect their final hardened versions.
-- ─────────────────────────────────────────────────────────────────────────

CREATE EXTENSION IF NOT EXISTS pgcrypto;   -- gen_random_uuid()

-- §4.1 enums (frozen, §25)
CREATE TYPE principal_type    AS ENUM ('platform_automation');   -- MVP; 'tenant_bot','user_pat' added post-launch (§2.4)
CREATE TYPE principal_status  AS ENUM ('active', 'revoked');
CREATE TYPE credential_status AS ENUM ('active', 'rotating', 'revoked');

-- ─────────────────────────────────────────────────────────────────────────
-- app_tenant_id() — reads the tenant GUC set by pgcommon's GUC bridge
-- (SET LOCAL app.tenant_id, RLS-6). STABLE, SECURITY DEFINER with pinned
-- search_path (§22 TS-D21: prevents definer-function search-path poisoning),
-- fail-closed (returns NULL on any error so RLS blocks rather than opens).
-- §4.3.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION app_tenant_id() RETURNS uuid
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public AS $$
DECLARE v text;
BEGIN
  v := current_setting('app.tenant_id', true);
  IF v IS NULL OR v = '' THEN RETURN NULL; END IF;
  RETURN v::uuid;
EXCEPTION WHEN OTHERS THEN RETURN NULL;
END;
$$;
REVOKE ALL ON FUNCTION app_tenant_id() FROM PUBLIC;

-- ─────────────────────────────────────────────────────────────────────────
-- log_rls_violation() — sampled INSERT into rls_violation_log. Sample rate
-- is app.rls_violation_sample_rate GUC when set (0..1; tests set 1), else
-- 1%. Swallows its own errors so a logging failure cannot abort the caller's
-- transaction. SECURITY DEFINER: callers need no grant on the table.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION log_rls_violation(p_table_name text, p_row_tenant_id uuid, p_violation_type text)
RETURNS void
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public AS $$
DECLARE v_rate double precision;
BEGIN
  BEGIN
    v_rate := COALESCE(NULLIF(current_setting('app.rls_violation_sample_rate', true), '')::double precision, 0.01);
  EXCEPTION WHEN OTHERS THEN
    v_rate := 0.01;
  END;
  IF random() >= v_rate THEN RETURN; END IF;
  INSERT INTO rls_violation_log (table_name, row_tenant_id, app_tenant_id, violation_type, query_text)
  VALUES (p_table_name, p_row_tenant_id, app_tenant_id(), p_violation_type, left(current_query(), 2048));
EXCEPTION WHEN OTHERS THEN
  NULL;
END;
$$;
REVOKE ALL ON FUNCTION log_rls_violation(text, uuid, text) FROM PUBLIC;

-- ─────────────────────────────────────────────────────────────────────────
-- rls_check_tenant() — the policy predicate used identically in USING and
-- WITH CHECK on both tenant-scoped tables. Logs which failure class occurred:
--   missing_or_invalid_guc — no/malformed app.tenant_id (RLS-6)
--   cross_tenant_access    — GUC set to a different tenant
-- §4.3.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION rls_check_tenant(p_tenant_id uuid, p_table_name text)
RETURNS boolean
LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
SET search_path = public AS $$
DECLARE v_app uuid;
BEGIN
  v_app := app_tenant_id();
  IF v_app IS NULL THEN
    PERFORM log_rls_violation(p_table_name, p_tenant_id, 'missing_or_invalid_guc');
    RETURN false;
  END IF;
  IF p_tenant_id <> v_app THEN
    PERFORM log_rls_violation(p_table_name, p_tenant_id, 'cross_tenant_access');
    RETURN false;
  END IF;
  RETURN true;
END;
$$;

-- ─────────────────────────────────────────────────────────────────────────
-- touch_row() — BEFORE UPDATE trigger on every record_version-carrying
-- table (§4.5). Fires only when the row actually changed so an idempotent
-- replay that produces an identical row does not bump record_version.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION touch_row() RETURNS trigger AS $$
BEGIN
  NEW.updated_at     := now();
  NEW.record_version := OLD.record_version + 1;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────
-- prune_rls_violation_log() — cmd/rotator's retention prune. SECURITY
-- DEFINER so the app role can batch-delete without a DELETE grant on the
-- audit table. Minimum 7-day retention (§22 TS-D21): a compromised app
-- credential cannot erase recent violation evidence.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION prune_rls_violation_log(p_ttl_days integer, p_limit integer)
RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public AS $$
DECLARE v_deleted integer;
BEGIN
  IF p_ttl_days < 7 THEN
    RAISE EXCEPTION 'prune_rls_violation_log: ttl_days must be at least 7 (got %)', p_ttl_days;
  END IF;
  IF p_limit < 1 THEN
    RAISE EXCEPTION 'prune_rls_violation_log: limit must be positive';
  END IF;
  DELETE FROM rls_violation_log
   WHERE id IN (SELECT id FROM rls_violation_log
                 WHERE occurred_at < now() - make_interval(days => p_ttl_days)
                 ORDER BY id LIMIT p_limit);
  GET DIAGNOSTICS v_deleted = ROW_COUNT;
  RETURN v_deleted;
END;
$$;
REVOKE ALL ON FUNCTION prune_rls_violation_log(integer, integer) FROM PUBLIC;

-- ═════════════════════════════════════════════════════════════════════════
-- §4.2 service_account_principals
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE service_account_principals (
  id                 uuid             NOT NULL DEFAULT gen_random_uuid(),
  tenant_id          uuid             NOT NULL,
  principal_sub      uuid             NOT NULL,
  keycloak_client_id text             NOT NULL,
  principal_type     principal_type   NOT NULL DEFAULT 'platform_automation',
  status             principal_status NOT NULL DEFAULT 'active',
  record_version     integer          NOT NULL DEFAULT 1 CHECK (record_version > 0),
  created_at         timestamptz      NOT NULL DEFAULT now(),
  updated_at         timestamptz      NOT NULL DEFAULT now(),
  deleted_at         timestamptz,
  CONSTRAINT service_account_principals_pkey PRIMARY KEY (id),
  CONSTRAINT uq_sap_id_tenant        UNIQUE (id, tenant_id),
  CONSTRAINT uq_sap_active_principal UNIQUE (tenant_id, principal_type)
);

CREATE INDEX idx_sap_tenant ON service_account_principals (tenant_id);
CREATE INDEX idx_sap_sub    ON service_account_principals (tenant_id, principal_sub);
CREATE INDEX idx_sap_status ON service_account_principals (tenant_id, status) WHERE deleted_at IS NULL;

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
-- Includes rotation-cadence columns (§16 TSQ-6 Resolved — cmd/scheduler).
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE service_account_credentials (
  id                    uuid              NOT NULL DEFAULT gen_random_uuid(),
  tenant_id             uuid              NOT NULL,
  principal_id          uuid              NOT NULL,
  version               integer           NOT NULL,
  status                credential_status NOT NULL DEFAULT 'active',
  openbao_path          text              NOT NULL,
  granted_by            uuid              NOT NULL,
  rotation_id           uuid,
  record_version        integer           NOT NULL DEFAULT 1 CHECK (record_version > 0),
  issued_at             timestamptz       NOT NULL DEFAULT now(),
  updated_at            timestamptz       NOT NULL DEFAULT now(),
  rotated_at            timestamptz,
  expires_at            timestamptz,
  revoked_at            timestamptz,
  deleted_at            timestamptz,
  rotation_cadence_days integer,
  next_rotation_at      timestamptz,
  CONSTRAINT service_account_credentials_pkey PRIMARY KEY (id),
  CONSTRAINT fk_sac_principal FOREIGN KEY (principal_id, tenant_id)
      REFERENCES service_account_principals (id, tenant_id) ON DELETE CASCADE,
  CONSTRAINT uq_sac_version UNIQUE (principal_id, version)
);

CREATE INDEX idx_sac_tenant    ON service_account_credentials (tenant_id);
CREATE INDEX idx_sac_principal ON service_account_credentials (principal_id, version DESC);
CREATE INDEX idx_sac_overlap   ON service_account_credentials (expires_at)
                                  WHERE status = 'rotating' AND expires_at IS NOT NULL;
CREATE INDEX idx_sac_next_rotation ON service_account_credentials (next_rotation_at)
  WHERE status = 'active' AND next_rotation_at IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX uq_sac_one_active
  ON service_account_credentials (principal_id) WHERE status = 'active' AND deleted_at IS NULL;
CREATE UNIQUE INDEX uq_sac_rotation_id
  ON service_account_credentials (principal_id, rotation_id) WHERE rotation_id IS NOT NULL;

CREATE TRIGGER service_account_credentials_touch_row BEFORE UPDATE ON service_account_credentials
  FOR EACH ROW WHEN (OLD.* IS DISTINCT FROM NEW.*) EXECUTE FUNCTION touch_row();

ALTER TABLE service_account_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE service_account_credentials FORCE  ROW LEVEL SECURITY;
REVOKE ALL ON service_account_credentials FROM PUBLIC;
CREATE POLICY service_account_credentials_rls ON service_account_credentials
  USING      (rls_check_tenant(tenant_id, 'service_account_credentials'))
  WITH CHECK (rls_check_tenant(tenant_id, 'service_account_credentials'));

-- ═════════════════════════════════════════════════════════════════════════
-- processed_events — dedup ledger for the inbound offboarding consumer
-- (§4.2, §7.1, §9.2). Operational and RLS-exempt.
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE processed_events (
  event_id     text        NOT NULL,
  consumer     text        NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (event_id, consumer)
);
CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

-- ═════════════════════════════════════════════════════════════════════════
-- rls_violation_log — audit trail for RLS check failures (§4.3, RLS-6).
-- Source of the iam_rls_violations_total metric. Operational, RLS-exempt.
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE rls_violation_log (
  id               bigserial   PRIMARY KEY,
  table_name       text        NOT NULL,
  row_tenant_id    uuid,
  app_tenant_id    uuid,
  violation_type   text        NOT NULL CHECK (violation_type IN ('missing_or_invalid_guc', 'cross_tenant_access')),
  session_role     text        DEFAULT SESSION_USER,
  application_name text        DEFAULT current_setting('application_name', true),
  query_text       text,
  occurred_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX rls_violation_log_occurred_at_idx ON rls_violation_log (occurred_at);
ALTER TABLE rls_violation_log DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON rls_violation_log FROM PUBLIC;

-- ═════════════════════════════════════════════════════════════════════════
-- keys_refresh_pending — durable "RP-17 key-cache refresh owed" marker
-- (EXT-6, TS-INV-7). intent_until = NULL is a committed marker; non-NULL
-- is cmd/scheduler's pre-rotation intent. Operational, RLS-exempt.
-- ═════════════════════════════════════════════════════════════════════════
CREATE TABLE keys_refresh_pending (
  tenant_id    uuid        PRIMARY KEY,
  requested_at timestamptz NOT NULL DEFAULT now(),
  intent_until timestamptz
);
CREATE INDEX keys_refresh_pending_requested_at_idx ON keys_refresh_pending (requested_at);
ALTER TABLE keys_refresh_pending DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON keys_refresh_pending FROM PUBLIC;

-- ═════════════════════════════════════════════════════════════════════════
-- outbox_events customization (MIG-1/MIG-2, §4.2/§4.4). platform-events'
-- outbox.ApplySchema creates outbox_events before this migration runs.
-- pgx encodes []byte as bytea hex in PgBouncer SimpleProtocol mode — text
-- accepts the raw bytes as-is. The trigger decodes it back on INSERT.
-- ═════════════════════════════════════════════════════════════════════════
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
    ALTER TABLE outbox_events ALTER COLUMN payload TYPE text USING payload::text;
  END IF;
END$$;

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
-- Roles & grants (§4.3, §4.4, RLS-1..RLS-7).
--   serviceaccount_app        — runtime (cmd/server, cmd/consumer). No BYPASSRLS.
--   serviceaccount_migrator   — runs this migration. BYPASSRLS so DDL is never
--                                blocked by FORCE RLS with no GUC set (§4.4).
--   serviceaccount_reconciler — cmd/rotator cross-tenant enumeration. BYPASSRLS,
--                                SELECT-only on service_account_* (RLS-7).
--   admin_readonly             — human operator/support SELECTs.
--
-- Roles are provisioned by infra (Terraform / test harness) ahead of this
-- migration. Guards skip gracefully when a role is absent or the current
-- user lacks ALTER ROLE privilege.
-- ═════════════════════════════════════════════════════════════════════════
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_migrator') THEN
    BEGIN
      ALTER ROLE serviceaccount_migrator BYPASSRLS;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'serviceaccount_migrator exists but current user cannot ALTER ROLE — skipping (fine in dev).';
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

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_reconciler') THEN
    BEGIN
      ALTER ROLE serviceaccount_reconciler BYPASSRLS;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'serviceaccount_reconciler exists but current user cannot ALTER ROLE — skipping (fine in dev).';
    END;
  END IF;

  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
    BEGIN
      ALTER ROLE admin_readonly BYPASSRLS;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'admin_readonly exists but current user cannot ALTER ROLE — skipping (fine in dev).';
    END;
  END IF;
END$$;

-- serviceaccount_app: full DML on all service and operational tables.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app') THEN
    RAISE NOTICE 'serviceaccount_app role missing — skipping grants.';
  ELSE
    BEGIN
      GRANT EXECUTE ON FUNCTION app_tenant_id()                           TO serviceaccount_app;
      GRANT EXECUTE ON FUNCTION rls_check_tenant(uuid, text)              TO serviceaccount_app;
      GRANT EXECUTE ON FUNCTION prune_rls_violation_log(integer, integer) TO serviceaccount_app;

      GRANT SELECT, INSERT, UPDATE, DELETE ON service_account_principals  TO serviceaccount_app;
      GRANT SELECT, INSERT, UPDATE, DELETE ON service_account_credentials TO serviceaccount_app;
      GRANT SELECT, INSERT, DELETE         ON processed_events            TO serviceaccount_app;
      GRANT SELECT, INSERT, UPDATE, DELETE ON keys_refresh_pending        TO serviceaccount_app;

      IF EXISTS (SELECT 1 FROM information_schema.tables
                 WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_events TO serviceaccount_app;
      END IF;
      IF EXISTS (SELECT 1 FROM information_schema.tables
                 WHERE table_schema = 'public' AND table_name = 'outbox_dead_letters') THEN
        GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_dead_letters TO serviceaccount_app;
      END IF;
      IF EXISTS (SELECT 1 FROM pg_class WHERE relkind = 'S' AND relname = 'outbox_events_ordering_seq') THEN
        GRANT USAGE, SELECT ON SEQUENCE outbox_events_ordering_seq TO serviceaccount_app;
      END IF;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to serviceaccount_app skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
END$$;

-- serviceaccount_reconciler: SELECT-only on service and operational tables (RLS-7).
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_reconciler') THEN
    RAISE NOTICE 'serviceaccount_reconciler role missing — skipping grants.';
  ELSE
    BEGIN
      GRANT SELECT ON service_account_principals  TO serviceaccount_reconciler;
      GRANT SELECT ON service_account_credentials TO serviceaccount_reconciler;
      GRANT SELECT ON rls_violation_log           TO serviceaccount_reconciler;
      GRANT SELECT ON keys_refresh_pending        TO serviceaccount_reconciler;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to serviceaccount_reconciler skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
END$$;

-- admin_readonly: SELECT on service and audit tables.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
    RAISE NOTICE 'admin_readonly role missing — skipping grants.';
  ELSE
    BEGIN
      GRANT SELECT ON service_account_principals  TO admin_readonly;
      GRANT SELECT ON service_account_credentials TO admin_readonly;
      GRANT SELECT ON rls_violation_log           TO admin_readonly;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
END$$;
