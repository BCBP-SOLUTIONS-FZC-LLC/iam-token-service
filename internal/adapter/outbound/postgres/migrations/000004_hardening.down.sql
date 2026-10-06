-- Restore 000003's prune (minimum 1 day) and 000001's app_tenant_id; drop
-- the dead-letter grants.
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
ALTER FUNCTION app_tenant_id() RESET search_path;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app') THEN
    BEGIN
      IF EXISTS (SELECT 1 FROM information_schema.tables
                 WHERE table_schema = 'public' AND table_name = 'outbox_dead_letters') THEN
        REVOKE SELECT, INSERT, UPDATE, DELETE ON outbox_dead_letters FROM serviceaccount_app;
      END IF;
      IF EXISTS (SELECT 1 FROM pg_class WHERE relkind = 'S' AND relname = 'outbox_events_ordering_seq') THEN
        REVOKE USAGE, SELECT ON SEQUENCE outbox_events_ordering_seq FROM serviceaccount_app;
      END IF;
    EXCEPTION WHEN insufficient_privilege THEN
      NULL;
    END;
  END IF;
END$$;
