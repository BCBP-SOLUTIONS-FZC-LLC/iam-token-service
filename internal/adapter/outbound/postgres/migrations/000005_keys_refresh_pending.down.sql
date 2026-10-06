-- Dropping the table drops its grants and index with it. Any marker still
-- pending is lost: trigger RP-17 by hand for those tenants before rolling
-- back (SELECT tenant_id FROM keys_refresh_pending).
DROP TABLE IF EXISTS keys_refresh_pending;
