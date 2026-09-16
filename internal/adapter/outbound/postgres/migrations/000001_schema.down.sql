-- ── outbox_events customization (reverse first — added last in up.sql) ──
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

-- admin_readonly is provisioned by infra ahead of the migration in prod
-- (see up.sql) and may legitimately not exist in dev/CI/a fresh
-- environment — REVOKE ... FROM <nonexistent role> raises "role does not
-- exist" (undefined_object) and aborts the whole statement, so this must
-- be guarded the same way up.sql's GRANT to admin_readonly is, or `migrate
-- down` fails outright anywhere that role was never created.
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

DROP TABLE IF EXISTS processed_events;

DROP TRIGGER IF EXISTS service_account_credentials_touch_row ON service_account_credentials;
DROP TABLE IF EXISTS service_account_credentials;

DROP TRIGGER IF EXISTS service_account_principals_touch_row ON service_account_principals;
DROP TABLE IF EXISTS service_account_principals;

DROP FUNCTION IF EXISTS touch_row();
DROP FUNCTION IF EXISTS rls_check_tenant(uuid, text);
DROP FUNCTION IF EXISTS app_tenant_id();

DROP TYPE IF EXISTS credential_status;
DROP TYPE IF EXISTS principal_status;
DROP TYPE IF EXISTS principal_type;
