#!/usr/bin/env bash
# PreToolUse guard (Edit|Write|MultiEdit): blocks edits that CI will reject.
# Exit 2 = block; stderr is fed back to Claude as the reason.
#   1. .env / go.sum  — protected files, never hand-edited.
#   2. MIG-1          — a migration already on origin/main is immutable.
#   3. RLS-6          — non-LOCAL SET app.tenant_id (check-set-local-only.sh gate).
#   4. pgcommon       — database/sql, pgxpool.New/Connect, pgx.Connect in production Go
#                       (check-pgcommon-observability.sh gate).
#   5. platform-events — sqs.ReceiveMessage / sns.Publish / sns.PublishBatch in
#                        production Go (check-platform-events-observability.sh gate).
#   6. TS-INV-1       — gocloak import anywhere (check-no-gocloak.sh gate).
#   7. gincommon-obs  — slog/stdlib-log/direct-zap/DefaultRegisterer/hand-rolled
#                       TracerProvider in production Go (check-gincommon-observability.sh gate).
#   8. arch-lint      — internal/core/* importing an adapter (except metrics in
#                       service layer); cmd/rotator importing internal/core/service.
# Only the text being written is checked (Write content, Edit new_string).
# Best-effort only — file changes via Bash are not seen; CI is the real gate.
set -euo pipefail
trap 'echo "guard-invariants.sh failed unexpectedly (line $LINENO); edit blocked. Install jq or fix the hook." >&2; exit 2' ERR

command -v jq >/dev/null || { echo "guard-invariants.sh needs jq (brew install jq); edit blocked." >&2; exit 2; }

input=$(cat)
file=$(jq -r '.tool_input.file_path // empty' <<<"$input")
[ -z "$file" ] && exit 0

# Normalise: absolute, symlinks resolved on the existing parent dir (the file
# itself may not exist yet), so /var vs /private/var and relative paths match.
case "$file" in /*) ;; *) file="${CLAUDE_PROJECT_DIR:-$PWD}/$file" ;; esac
dir=$(dirname "$file")
while [ ! -d "$dir" ]; do dir=$(dirname "$dir"); done
file="$(cd "$dir" && pwd -P)${file#"$dir"}"
path="/${file#/}"
base=$(basename "$path")

content=$(jq -r '[.tool_input.content, .tool_input.new_string, (.tool_input.edits // [] | .[].new_string)] | map(select(. != null)) | join("\n")' <<<"$input")

block() { echo "Blocked ($1): $2" >&2; exit 2; }

# ── 1. Protected files ───────────────────────────────────────────────────────
[[ "$base" == ".env" ]] && \
  block ".env" ".env holds local config/secrets and is not edited by Claude. Edit .env-example instead."

[[ "$base" == "go.sum" ]] && \
  block "go.sum" "go.sum is managed by 'go mod tidy' — never hand-edit it."

# ── 2. MIG-1: immutable migrations ──────────────────────────────────────────
if [[ "$path" == */migrations/*.sql ]]; then
  if top=$(git -C "$dir" rev-parse --show-toplevel 2>/dev/null); then
    top=$(cd "$top" && pwd -P)
    rel="${file#"$top"/}"
    if git -C "$top" cat-file -e "origin/main:$rel" 2>/dev/null; then
      block "MIG-1" "$base is already on origin/main; migrations are additive only. Add a new 000NNN_<name>.{up,down}.sql instead of changing it."
    fi
  fi
fi

# ── Classify production Go ───────────────────────────────────────────────────
is_prod_go=false
if [[ "$path" == *.go && "$path" != *_test.go \
    && ( "$path" == */cmd/* || "$path" == */internal/* || "$path" == */pkg/* ) \
    && "$path" != */test/* ]]; then
  is_prod_go=true
fi

# ── 3. RLS-6: non-LOCAL SET app.tenant_id ───────────────────────────────────
# Matches the regex in check-set-local-only.sh; excludes set_config (the
# pgcommon-sanctioned path) and SET LOCAL.
if [[ ( "$path" == *.go || "$path" == *.sql ) \
    && ( "$path" == */cmd/* || "$path" == */internal/* || "$path" == */pkg/* ) ]] \
  && grep -E '\bSET[[:space:]]+("?app\.tenant_id"?)[[:space:]]*=' <<<"$content" \
     | grep -v 'SET LOCAL' | grep -qv 'set_config'; then
  block "RLS-6" "a session-level SET app.tenant_id leaks the tenant across PgBouncer backends. Use pgcommon.GUCSetFromContext / WithGUCSet — it emits SELECT set_config('app.tenant_id', \$1, true) inside a transaction (SET LOCAL semantics)."
fi

if $is_prod_go; then
  # ── 4. pgcommon: no raw pool/connect ──────────────────────────────────────
  if grep -q '"database/sql"' <<<"$content"; then
    block "pgcommon-obs" "database/sql bypasses the pgcommon pool, RLS GUC binding, and error normalisation. Use pgcommon.NewPool (internal/adapter/outbound/postgres)."
  fi
  if grep -Eq '\bpgxpool\.(New|NewWithConfig|Connect)[[:space:]]*\(|\bpgx\.Connect(Config)?[[:space:]]*\(' <<<"$content"; then
    block "pgcommon-obs" "construct pools only via pgcommon.NewPool (internal/adapter/outbound/postgres/db.go)."
  fi

  # ── 5. platform-events: no direct SQS/SNS bypass ──────────────────────────
  # Matches check-platform-events-observability.sh exactly.
  # sqs.SendMessage (DLQ routing in cmd/consumer/dlq.go) and
  # glue.GetSchemaByDefinition (Glue codec) are allowed.
  if grep -Eq 'sqs\.ReceiveMessage\(|sns\.Publish\(|sns\.PublishBatch\(' <<<"$content"; then
    block "platform-events-obs" "direct SQS/SNS calls bypass platform-events. Use events.NewSQSConsumerWithClient for consumption and outbox.Enqueue via port.EventPublisher for publishing (cmd/server's outbox.Runner relays to SNS)."
  fi

  # ── 6. TS-INV-1: no gocloak import ────────────────────────────────────────
  # This service is a credential-lifecycle owner only; Realm Provisioner is
  # the sole Keycloak Admin API writer (TS-INV-1 / check-no-gocloak.sh).
  if grep -q '"github.com/Nerzal/gocloak' <<<"$content"; then
    block "TS-INV-1" "gocloak import is forbidden — this service never writes to Keycloak. Realm Provisioner owns the Keycloak Admin API (TS-INV-1)."
  fi

  # ── 7. gincommon-obs: no direct slog/log/zap/DefaultRegisterer/TracerProvider
  # Matches check-gincommon-observability.sh exactly. All observability must
  # enter through platform-gincommon (logger.NewLogger, metrics.Register,
  # gincommon.InitTracingWithConfig).
  if grep -Eq \
    'log/slog|"log"|go\.uber\.org/zap|zap\.New\(|zap\.L\(\)|zap\.S\(\)|zap\.ReplaceGlobals\(|zapcore\.NewCore\(|slog\.|log\.(Print|Fatal|Panic|Printf)|prometheus\.DefaultRegisterer|prometheus\.MustRegister\(|prometheus/promauto|otel\.SetTracerProvider|sdktrace\.NewTracerProvider|sdktrace\.NewBatchSpanProcessor\(|promhttp\.Handler\(\)|InitTracingFromEnv\(\)' \
    <<<"$content"; then
    block "gincommon-obs" "observability must go through platform-gincommon: use logger.NewLogger/port.Logger for logs, metrics.Register/gincommon.MetricsRegisterer for metrics, gincommon.InitTracingWithConfig for traces."
  fi

  # ── 8a. arch-lint: internal/core/* must not import adapters ───────────────
  if [[ "$path" == */internal/core/* ]]; then
    hits=$(grep -Eo '"github\.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/[^"]*"' <<<"$content" || true)
    # service layer may import the metrics adapter (allowed by .go-arch-lint.yml)
    if [[ "$path" == */internal/core/service/* ]]; then
      hits=$(grep -v '/internal/adapter/outbound/metrics"' <<<"$hits" || true)
    fi
    if [ -n "$hits" ]; then
      block "arch-lint" "internal/core must not import adapters ($(tr '\n' ' ' <<<"$hits")). Add a port interface in internal/core/port and implement it in the adapter."
    fi
  fi

  # ── 8b. arch-lint: cmd/rotator must not import internal/core/service ───────
  # rotator enumerates across tenants on the BYPASSRLS reconciler pool and
  # writes through the repository layer only (TS-D14 / architecture.md).
  if [[ "$path" == */cmd/rotator/* ]]; then
    if grep -q '"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service' <<<"$content"; then
      block "arch-lint" "cmd/rotator must not import internal/core/service — write through the repository layer only (TS-D14)."
    fi
  fi
fi

exit 0
