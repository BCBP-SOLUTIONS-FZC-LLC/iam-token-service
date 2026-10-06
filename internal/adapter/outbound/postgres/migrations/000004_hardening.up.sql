-- ═════════════════════════════════════════════════════════════════════════
-- Production-readiness hardening (LLD §22 TS-D21).
-- ═════════════════════════════════════════════════════════════════════════

-- 1. Outbox dead-lettering. platform-events' outbox runner moves an event
--    that exhausted OUTBOX_MAX_ATTEMPTS into outbox_dead_letters (INSERT +
--    DELETE FROM outbox_events in one transaction) and lists/reprocesses/
--    discards dead letters — all as serviceaccount_app. 000001 granted only
--    outbox_events, so that move failed with permission denied and the
--    poison event was retried forever. Both tables are created by
--    outbox.ApplySchema before this migration runs (MIG-2), hence IF EXISTS.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app') THEN
    RAISE NOTICE 'serviceaccount_app role missing — skipping grants.';
    RETURN;
  END IF;
  BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables
               WHERE table_schema = 'public' AND table_name = 'outbox_dead_letters') THEN
      GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_dead_letters TO serviceaccount_app;
    END IF;
    -- outbox.EnqueueOrdered and dead-letter replay draw ordering_seq values.
    IF EXISTS (SELECT 1 FROM pg_class WHERE relkind = 'S' AND relname = 'outbox_events_ordering_seq') THEN
      GRANT USAGE, SELECT ON SEQUENCE outbox_events_ordering_seq TO serviceaccount_app;
    END IF;
  EXCEPTION WHEN insufficient_privilege THEN
    RAISE NOTICE 'Grants to serviceaccount_app skipped — current user (%) lacks GRANT privilege.', current_user;
  END;
END$$;

-- 2. app_tenant_id() is SECURITY DEFINER: pin its search_path (a definer
--    function must not resolve names through the caller's search_path) and
--    keep it off PUBLIC. Behaviour is unchanged.
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
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app') THEN
    GRANT EXECUTE ON FUNCTION app_tenant_id() TO serviceaccount_app;
  END IF;
END$$;

-- 3. prune_rls_violation_log: a server-side minimum retention of 7 days, so a
--    compromised app credential cannot erase recent RLS-violation evidence
--    (it previously accepted ttl_days >= 1).
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
