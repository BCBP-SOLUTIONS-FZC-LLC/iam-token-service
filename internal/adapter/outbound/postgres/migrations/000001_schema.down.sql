-- ─────────────────────────────────────────────────────────────────────────
-- Reverse of consolidated 000001_schema.up.sql.
-- Drop order: operational tables → service tables → functions → types.
-- ─────────────────────────────────────────────────────────────────────────

-- keys_refresh_pending
DROP TABLE IF EXISTS keys_refresh_pending;

-- rls_violation_log and its functions
DROP FUNCTION IF EXISTS prune_rls_violation_log(integer, integer);
DROP FUNCTION IF EXISTS log_rls_violation(text, uuid, text);
DROP TABLE IF EXISTS rls_violation_log;

-- outbox_events customization (reverse: drop trigger, revert payload to jsonb)
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
    DROP TRIGGER IF EXISTS trg_outbox_normalize_payload ON outbox_events;
    REVOKE ALL ON outbox_events FROM serviceaccount_app;
  END IF;
END$$;
DROP FUNCTION IF EXISTS outbox_normalize_payload();
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables
             WHERE table_schema = 'public' AND table_name = 'outbox_events') THEN
    ALTER TABLE outbox_events ALTER COLUMN payload TYPE jsonb USING payload::jsonb;
  END IF;
END$$;

-- Revoke grants (admin_readonly may not exist — guard against undefined_object)
DO $$
BEGIN
  REVOKE ALL ON processed_events            FROM serviceaccount_app, serviceaccount_reconciler, admin_readonly;
  REVOKE ALL ON service_account_credentials FROM serviceaccount_app, serviceaccount_reconciler, admin_readonly;
  REVOKE ALL ON service_account_principals  FROM serviceaccount_app, serviceaccount_reconciler, admin_readonly;
EXCEPTION WHEN undefined_object THEN
  RAISE NOTICE 'admin_readonly role missing — revoking only from serviceaccount_app/serviceaccount_reconciler.';
  REVOKE ALL ON processed_events            FROM serviceaccount_app, serviceaccount_reconciler;
  REVOKE ALL ON service_account_credentials FROM serviceaccount_app, serviceaccount_reconciler;
  REVOKE ALL ON service_account_principals  FROM serviceaccount_app, serviceaccount_reconciler;
END$$;

-- processed_events
DROP TABLE IF EXISTS processed_events;

-- service_account_credentials (triggers dropped with the table)
DROP TRIGGER IF EXISTS service_account_credentials_touch_row ON service_account_credentials;
DROP TABLE IF EXISTS service_account_credentials;

-- service_account_principals (triggers dropped with the table)
DROP TRIGGER IF EXISTS service_account_principals_touch_row ON service_account_principals;
DROP TABLE IF EXISTS service_account_principals;

-- shared functions
DROP FUNCTION IF EXISTS touch_row();
DROP FUNCTION IF EXISTS rls_check_tenant(uuid, text);
DROP FUNCTION IF EXISTS app_tenant_id();

-- enums
DROP TYPE IF EXISTS credential_status;
DROP TYPE IF EXISTS principal_status;
DROP TYPE IF EXISTS principal_type;
