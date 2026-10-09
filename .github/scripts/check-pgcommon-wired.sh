#!/usr/bin/env bash
# Positive assertion: every cmd binary wires all database connections,
# configuration, and operations through platform-pgcommon.
#
# This gate catches a binary added or refactored without the required
# pgcommon initialization — an absence the negative gate cannot detect.
#
# Required per binary (cmd/server, cmd/consumer, cmd/rotator, cmd/scheduler):
#
#   CONFIGURATION (one env-driven DSN, not hand-assembled strings)
#     pgcommon.ConfigFromEnv()        — reads DATABASE_URL/PG_* env vars;
#                                       the single authoritative DSN assembly
#     pgadapter.DSNFromEnv()          — delegates to ConfigFromEnv for the
#                                       app-pool DSN (TS-CONFIG-1)
#
#   CONNECTION POOL
#     pgcommon.NewPool(               — every pool created through pgcommon;
#                                       registers platform_db_* metrics
#
#   RLS TENANT INJECTION
#     pgcommon.GUCSetFromContext      — GUCProvider that injects app.tenant_id
#                                       as SET LOCAL on every connection checkout;
#                                       mandatory for the RLS-scoped app pool
#                                       (RLS-6/§4.3; absence means all queries
#                                       run without tenant isolation)
#
#   DATABASE LOGGING
#     pgadapter.NewLoggerAdapter(     — routes pgcommon slow-query / migration
#                                       log output through port.Logger instead
#                                       of a separate sink (also checked by
#                                       check-gincommon-wired.sh)
#
#   MIGRATIONS (applies to server; consumer/rotator/scheduler check RUN_MIGRATIONS)
#     pgadapter.MigrationDSNFromEnv() — migration DSN bypasses PgBouncer
#                                       (TS-CONFIG-2: pg_advisory_lock is
#                                       session-scoped, breaks under transaction
#                                       pooling); MIGRATION_DATABASE_URL must
#                                       be set whenever PG_BOUNCER_MODE=true
#
# Grep searches the whole cmd/<binary>/ directory so the check survives
# internal refactors that move wiring into helper files.
set -euo pipefail

BINARIES=(server consumer rotator scheduler)
FAIL=0

check_required() {
  local bin="$1" pattern="$2" label="$3"
  if ! grep -rqE "$pattern" "cmd/${bin}/" --include='*.go' --exclude='*_test.go'; then
    echo "FAIL: cmd/${bin}: missing ${label}"
    FAIL=1
  fi
}

echo "Checking that every binary wires database connections/config/operations through platform-pgcommon (positive gate)..."

for bin in "${BINARIES[@]}"; do
  # ── Configuration ─────────────────────────────────────────────────────────
  check_required "$bin" \
    'pgcommon\.ConfigFromEnv\(\)' \
    "pgcommon.ConfigFromEnv() (single env-driven DSN/pool-sizing source — no hand-assembled configs)"
  check_required "$bin" \
    'pgadapter\.DSNFromEnv\(\)|pgadapter\.MigrationDSNFromEnv\(\)|pgadapter\.ReconcilerDSNFromEnv\(\)' \
    "pgadapter.DSNFromEnv / MigrationDSNFromEnv / ReconcilerDSNFromEnv (DSN delegated to ConfigFromEnv, not raw env var reads)"

  # ── Connection pool ────────────────────────────────────────────────────────
  check_required "$bin" \
    'pgcommon\.NewPool\(' \
    "pgcommon.NewPool (all pools created via pgcommon; registers platform_db_* metrics)"

  # ── RLS tenant injection ───────────────────────────────────────────────────
  check_required "$bin" \
    'pgcommon\.GUCSetFromContext' \
    "pgcommon.GUCSetFromContext (GUCProvider — injects SET LOCAL app.tenant_id on every app-pool checkout; RLS-6)"

  # ── Database logging ───────────────────────────────────────────────────────
  check_required "$bin" \
    'pgadapter\.NewLoggerAdapter\(' \
    "pgadapter.NewLoggerAdapter (pgcommon slow-query/migration logs routed through port.Logger)"
done

if [ "$FAIL" -ne 0 ]; then
  echo ""
  echo "FAIL: one or more cmd binaries are missing required pgcommon database wiring."
  echo "Every binary must call all five patterns above; see CLAUDE.md §'Database' and the §4 pool-setup conventions."
  exit 1
fi
echo "OK: all four cmd binaries (server consumer rotator scheduler) wire database through platform-pgcommon"
