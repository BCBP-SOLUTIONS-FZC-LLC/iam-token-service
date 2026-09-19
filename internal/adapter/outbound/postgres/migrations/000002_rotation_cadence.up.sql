-- ═════════════════════════════════════════════════════════════════════════
-- §16 TSQ-6 Resolved — automatic rotation-cadence scheduler (cmd/scheduler).
-- Adds cadence state to service_account_credentials so a scheduler can
-- enumerate which principals are due for rotation and TS-3 can surface it.
-- ═════════════════════════════════════════════════════════════════════════
ALTER TABLE service_account_credentials
  ADD COLUMN rotation_cadence_days integer,   -- cadence in effect when this row was issued/rotated (TS-D10, default 90)
  ADD COLUMN next_rotation_at      timestamptz; -- when this 'active' row is next due; NULL once superseded (rotating/revoked) or for pre-migration rows

-- The cadence scheduler's due-list scan (cmd/scheduler) — mirrors
-- idx_sac_overlap's existing partial-index pattern for the overlap sweep.
CREATE INDEX idx_sac_next_rotation ON service_account_credentials (next_rotation_at)
  WHERE status = 'active' AND next_rotation_at IS NOT NULL AND deleted_at IS NULL;
