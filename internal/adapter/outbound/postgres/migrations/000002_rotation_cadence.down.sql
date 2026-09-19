DROP INDEX IF EXISTS idx_sac_next_rotation;

ALTER TABLE service_account_credentials
  DROP COLUMN IF EXISTS next_rotation_at,
  DROP COLUMN IF EXISTS rotation_cadence_days;
