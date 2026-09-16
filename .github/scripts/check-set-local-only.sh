#!/usr/bin/env bash
# RLS-6: the app.tenant_id GUC must only ever be set transaction-locally.
# The only sanctioned path in this service is via platform-pgcommon's
# GUCSetFromContext / WithGUCSet, which issues
# `SELECT set_config('app.tenant_id', $1, true)` — transaction-local
# (is_local=true) — inside a transaction. A session-scoped
# `SET app.tenant_id = ...` would persist across pooled backends and leak
# across tenants under PgBouncer-style connection reuse (§4.3), exactly
# what RLS-6 and the rls_test.go canonical matrix guard against.
set -euo pipefail

echo "Checking RLS-6 (SET LOCAL-only tenant GUC binding)..."

hits=$(grep -REn '\bSET[[:space:]]+("?app\.tenant_id"?)[[:space:]]*=' \
         --include='*.go' --include='*.sql' \
         cmd/ internal/ pkg/ 2>/dev/null | grep -v 'SET LOCAL' | grep -v 'set_config' || true)

if [ -n "$hits" ]; then
  echo "FAIL: non-transaction-local SET app.tenant_id detected (RLS-6 violation):"
  echo "$hits"
  echo "Use pgcommon.WithGUCSet / GUCSetFromContext, which emits"
  echo "SELECT set_config('app.tenant_id', \$1, true) inside a transaction (SET LOCAL semantics)."
  exit 1
fi
echo "OK: no forbidden non-LOCAL SET app.tenant_id found"
