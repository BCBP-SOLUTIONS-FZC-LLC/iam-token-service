-- Restore 000001's non-logging predicate before dropping what it calls.
CREATE OR REPLACE FUNCTION rls_check_tenant(p_tenant_id uuid, p_table_name text)
RETURNS boolean
LANGUAGE plpgsql STABLE STRICT SECURITY DEFINER
SET search_path = public AS $$
DECLARE v_app uuid;
BEGIN
  v_app := app_tenant_id();
  IF v_app IS NULL     THEN RETURN false; END IF;
  IF p_tenant_id <> v_app THEN RETURN false; END IF;
  RETURN true;
END;
$$;

DROP FUNCTION IF EXISTS prune_rls_violation_log(integer, integer);
DROP FUNCTION IF EXISTS log_rls_violation(text, uuid, text);
DROP TABLE IF EXISTS rls_violation_log;
