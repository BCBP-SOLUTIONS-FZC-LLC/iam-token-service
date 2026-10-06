-- ═════════════════════════════════════════════════════════════════════════
-- keys_refresh_pending — the durable "RP-17 key-cache refresh owed for this
-- tenant" marker (EXT-6, TS-INV-7). Under the client-jwt/JWKS mechanism a
-- revoked or superseded key keeps authenticating at Keycloak until the
-- Realm Provisioner's ClearServiceAccountKeysCache (RP-17) lands; there is
-- no Keycloak-side TTL. cmd/rotator's sweep writes the marker in the SAME
-- transaction as each revoke, and cmd/scheduler writes it in its own
-- transaction BEFORE each IssueOrRotate (intent-first: a spurious extra
-- RP-17 is harmless, a missed one is not). Each binary deletes the marker
-- only after RP-17 succeeds, and only when requested_at has not moved past
-- the value it observed — so a newer request is never cleared — and retries
-- every leftover marker at the start of its next run. A crash, SIGKILL or
-- an RP-17 outage between the commit and the refresh is therefore
-- recoverable instead of silently lost.
--
-- intent_until distinguishes the two kinds of marker. NULL is a COMMITTED
-- marker: the revoke/rotation it covers has committed, so any run may retry
-- the refresh at once. A non-NULL value is cmd/scheduler's INTENT marker,
-- written before IssueOrRotate may commit: the retry pass leaves it alone
-- until intent_until has passed (calling RP-17 before the rotation commits
-- would refresh nothing, and clearing the marker would then forget the
-- refresh owed), by which time its writer has either converted it into a
-- committed marker, cleared it, or died. A committed write always wins: it
-- sets intent_until back to NULL, and an intent write never downgrades an
-- existing committed marker.
--
-- Operational, RLS-exempt (like processed_events): one row per tenant,
-- never tenant-queried, written only by the two CronJob binaries.
-- ═════════════════════════════════════════════════════════════════════════

CREATE TABLE keys_refresh_pending (
  tenant_id    uuid        PRIMARY KEY,
  requested_at timestamptz NOT NULL DEFAULT now(),
  intent_until timestamptz
);

-- The retry pass reads oldest-first.
CREATE INDEX keys_refresh_pending_requested_at_idx ON keys_refresh_pending (requested_at);

ALTER TABLE keys_refresh_pending DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON keys_refresh_pending FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_app') THEN
    BEGIN
      GRANT SELECT, INSERT, UPDATE, DELETE ON keys_refresh_pending TO serviceaccount_app;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to serviceaccount_app skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  ELSE
    RAISE NOTICE 'serviceaccount_app role missing — skipping grants.';
  END IF;
  -- The retry pass's cross-tenant enumeration (RLS-7: SELECT only).
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'serviceaccount_reconciler') THEN
    BEGIN
      GRANT SELECT ON keys_refresh_pending TO serviceaccount_reconciler;
    EXCEPTION WHEN insufficient_privilege THEN
      RAISE NOTICE 'Grants to serviceaccount_reconciler skipped — current user (%) lacks GRANT privilege.', current_user;
    END;
  ELSE
    RAISE NOTICE 'serviceaccount_reconciler role missing — skipping grants.';
  END IF;
END$$;
