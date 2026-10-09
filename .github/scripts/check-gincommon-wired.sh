#!/usr/bin/env bash
# Positive assertion: every cmd binary actually wires all three observability
# pillars (logs, metrics, traces) through platform-gincommon.
#
# This gate catches a binary that was added or refactored without calling the
# required gincommon initialization functions — patterns the negative gate
# (check-gincommon-observability.sh) cannot detect because they are absences,
# not forbidden usages.
#
# Required per binary (cmd/server, cmd/consumer, cmd/rotator, cmd/scheduler):
#
#   LOGS
#     logger.NewLogger       — the single Zap-backed port.Logger for the process
#     pgadapter.NewLoggerAdapter — routes pgcommon slow-query / migration logs
#                                  through port.Logger instead of pgcommon's default
#
#   METRICS (Standard §"Implementation Requirements")
#     gincommon.ObservabilityMiddlewares — initialises MetricsRegisterer and injects
#                                          {domain,service,environment} const labels
#     metrics.InitLibraryMetrics         — registers platform-events / platform-pgcommon
#                                          Tier 1 collectors on the shared registerer
#     metrics.Register                   — registers this service's Tier 2/3 collectors
#     gincommon.MetricsHandler           — serves /metrics on METRICS_PORT
#
#   TRACES
#     gincommon.InitTracingWithConfig — installs the OTLP TracerProvider with the
#                                       same identity as metrics (service, domain, env)
#     gincommon.NewSpanTracer         — wires pgcommon pool spans into gincommon's
#                                       TracerProvider (not a bare otel.Tracer)
#
# Grep searches the whole cmd/<binary>/ directory (not just main.go) so the
# check survives internal refactors that move wiring into helper files.
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

echo "Checking that every binary wires logs/metrics/traces through platform-gincommon (positive gate)..."

for bin in "${BINARIES[@]}"; do
  # ── Logs ──────────────────────────────────────────────────────────────────
  check_required "$bin" \
    'logger\.NewLogger\(' \
    "logger.NewLogger (Zap logger from gincommon — the single port.Logger for this process)"
  check_required "$bin" \
    'pgadapter\.NewLoggerAdapter\(' \
    "pgadapter.NewLoggerAdapter (pgcommon logs routed through port.Logger, not pgcommon default)"

  # ── Metrics ───────────────────────────────────────────────────────────────
  check_required "$bin" \
    'gincommon\.ObservabilityMiddlewares\(' \
    "gincommon.ObservabilityMiddlewares (MetricsRegisterer init + {domain,service,environment} labels)"
  check_required "$bin" \
    'metrics\.InitLibraryMetrics\(' \
    "metrics.InitLibraryMetrics (platform-events / platform-pgcommon Tier 1 collectors)"
  check_required "$bin" \
    'metrics\.Register\(' \
    "metrics.Register (Tier 2/3 business metrics on gincommon.MetricsRegisterer)"
  check_required "$bin" \
    'gincommon\.MetricsHandler\(' \
    "gincommon.MetricsHandler (/metrics endpoint — safe collector-failure isolation)"

  # ── Traces ────────────────────────────────────────────────────────────────
  check_required "$bin" \
    'gincommon\.InitTracingWithConfig\(' \
    "gincommon.InitTracingWithConfig (TracerProvider with matching service/domain/env identity)"
  check_required "$bin" \
    'gincommon\.NewSpanTracer\(' \
    "gincommon.NewSpanTracer (pgcommon pool tracer — DB spans on gincommon TracerProvider)"
done

if [ "$FAIL" -ne 0 ]; then
  echo ""
  echo "FAIL: one or more cmd binaries are missing required gincommon observability wiring."
  echo "Every binary must call all eight functions above; see docs/observability/README.md §'Platform libraries'."
  exit 1
fi
echo "OK: all four cmd binaries (server consumer rotator scheduler) wire logs/metrics/traces through platform-gincommon"
