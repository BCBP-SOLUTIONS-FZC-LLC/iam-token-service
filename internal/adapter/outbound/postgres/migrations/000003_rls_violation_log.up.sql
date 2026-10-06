-- ═════════════════════════════════════════════════════════════════════════
-- RLS violation audit trail (§4.3, RLS-6) — the source of the Tier 2
-- iam_rls_violations_total{violation_type} metric, matching
-- iam-org-membership / iam-user-profile / iam-audit-log.
--
-- rls_check_tenant() already fails closed; this migration only makes the
-- failures visible. Each failed check is sampled into rls_violation_log by
-- log_rls_violation(); cmd/server's exporter counts new rows over the
-- BYPASSRLS reconciler pool and increments the counter. The table is
-- operational (RLS-exempt), like processed_events.
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

-- RLS stays off: log_rls_violation() is called from inside RLS policy
-- checks, so a policy on this table would recurse.
ALTER TABLE rls_violation_log DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON rls_violation_log FROM PUBLIC;

-- ─────────────────────────────────────────────────────────────────────────
-- log_rls_violation() — sampled INSERT into rls_violation_log. The sample
-- rate is the app.rls_violation_sample_rate GUC when set (0..1; tests set
-- 1), else 1%. Swallows its own errors so a logging failure can never abort
-- the caller's transaction. SECURITY DEFINER: callers need no grant on the
-- table.
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

-- ─────────────────────────────────────────────────────────────────────────
-- rls_check_tenant() — same predicate as 000001 (fail closed on a missing
-- or malformed GUC, hide/reject another tenant's row), now logging which
-- case failed:
--   missing_or_invalid_guc — no/malformed app.tenant_id (RLS-6)
--   cross_tenant_access    — GUC set to a different tenant
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
-- prune_rls_violation_log() — cmd/rotator's retention prune. SECURITY
-- DEFINER so the RLS-scoped app role can delete only rows past retention,
-- in batches, without holding a DELETE grant on the audit table.
-- ─────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION prune_rls_violation_log(p_ttl_days integer, p_limit integer)
RETURNS integer
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public AS $$
DECLARE v_deleted integer;
BEGIN
  IF p_ttl_days < 1 OR p_limit < 1 THEN
    RAISE EXCEPTION 'prune_rls_violation_log: ttl_days and limit must be positive';
  END IF;
  DELETE FROM rls_violation_log
   WHERE id IN (SELECT id FROM rls_violation_log
                 WHERE occurred_at < now() - make_interval(days => p_ttl_days)
                 ORDER BY id LIMIT p_limit);
  GET DIAGNOSTICS v_deleted = ROW_COUNT;
  RETURN v_deleted;
END;
$$;

REVOKE ALL ON FUNCTION log_rls_violation(text, uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION prune_rls_violation_log(integer, integer) FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app') THEN
    BEGIN
      GRANT EXECUTE ON FUNCTION prune_rls_violation_log(integer, integer) TO serviceaccount_app;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to serviceaccount_app skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
  -- The exporter's cross-tenant read (RLS-7: SELECT only).
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_reconciler') THEN
    BEGIN
      GRANT SELECT ON rls_violation_log TO serviceaccount_reconciler;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to serviceaccount_reconciler skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'admin_readonly') THEN
    BEGIN
      GRANT SELECT ON rls_violation_log TO admin_readonly;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to admin_readonly skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  END IF;
END$$;
