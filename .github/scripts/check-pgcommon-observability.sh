#!/usr/bin/env bash
# Database access must enter the process only through platform-pgcommon,
# matching iam-user-profile / iam-org-membership (every binary:
# cmd/server, cmd/consumer, cmd/rotator, cmd/scheduler):
#
#   connections   — pgcommon.NewPool (never pgxpool.Connect, pgxpool.New,
#                   pgx.Connect, or pgx.ConnectConfig)
#   configuration — pgcommon.ConfigFromEnv / pgadapter.SystemPoolConfig
#                   (never hand-assembled pgxpool.Config or pgx.ConnConfig)
#   transactions  — pgcommon.RunInTx / pgcommon.RunInTxWithRetryOpts /
#                   withPool (never pgx.Conn.Begin or pgxpool.Pool.Begin)
#   SQL driver    — pgcommon/pgx only (never database/sql, sql.Open)
#   direct Conn   — pgxpool.Conn must NOT appear in production code; all
#                   query execution goes through pgx.Tx obtained via pgcommon
#
# Allowed patterns (pgcommon transitive-dependency error inspection):
#   pgconn.PgError — SQLSTATE inspection in db.go and errors.go
#   puddle.ErrClosedPool — closed-pool detection in db.go
#   pgx.Tx / pgx.TxOptions / pgx.CollectRows — pgcommon exposes these types
#
# This is the NEGATIVE gate: it rejects forbidden patterns in cmd/ and
# internal/. The test/ directory is intentionally excluded — test seeding
# infrastructure (test/dbseed/) wraps pgcommon.Pool with pgxpool-like helpers
# for compatibility with test helpers, which is not a production bypass.
# The companion check-pgcommon-wired.sh is the POSITIVE gate.
set -euo pipefail

echo "Checking database connections, config, and operations go through platform-pgcommon (negative gate)..."

offenders=$(grep -rnE \
  'pgxpool\.Connect\(|pgxpool\.New\(|pgx\.Connect\(|pgx\.ConnectConfig\(|pgxpool\.Pool\b|pgxpool\.Conn\b|"database/sql"|sql\.Open\(' \
  --include='*.go' \
  --exclude='*_test.go' \
  cmd/ internal/ 2>/dev/null || true)

if [ -n "$offenders" ]; then
	echo "FAIL: direct database connection/pool usage bypasses platform-pgcommon:"
	echo "  connections → use pgcommon.NewPool (config via pgcommon.ConfigFromEnv / pgadapter.SystemPoolConfig)"
	echo "  transactions → use pgcommon.RunInTx / pgcommon.RunInTxWithRetryOpts / withPool helper"
	echo "  SQL driver  → pgcommon/pgx only (never database/sql or sql.Open)"
	echo "  direct Conn → pgxpool.Conn must NOT be in production code; use pgx.Tx from pgcommon.RunInTx"
	echo ""
	echo "$offenders"
	exit 1
fi
echo "OK: no raw pgxpool.Connect/pgx.Connect/database/sql/pgxpool.Conn in production code (cmd/, internal/)"
