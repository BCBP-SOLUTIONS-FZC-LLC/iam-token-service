#!/usr/bin/env bash
# Observability must enter the process only through platform-gincommon,
# matching iam-user-profile / iam-org-membership (every binary:
# cmd/server, cmd/consumer, cmd/rotator, cmd/scheduler):
#   logs    — Zap via logger.NewLogger (port.Logger), never slog / stdlib log /
#             zap.New / zap.L() / zap.S() / zap.ReplaceGlobals / zapcore.NewCore
#   metrics — gincommon.MetricsRegisterer / MetricsHandler, never
#             prometheus.DefaultRegisterer / promauto / bare promhttp.Handler /
#             prometheus.MustRegister (uses DefaultRegisterer)
#   traces  — gincommon.InitTracingWithConfig (same identity as metrics), never
#             InitTracingFromEnv / otel.SetTracerProvider / sdktrace.NewTracerProvider /
#             sdktrace.NewBatchSpanProcessor (implies a hand-rolled pipeline)
#
# This is the NEGATIVE gate: it rejects forbidden import paths and call patterns.
# The companion check-gincommon-wired.sh is the POSITIVE gate: it asserts that
# every binary actually calls all the required gincommon initialization functions.
set -euo pipefail

echo "Checking logs/metrics/traces go through platform-gincommon (negative gate)..."

offenders=$(grep -rnE \
  'log/slog|"log"|go\.uber\.org/zap|zap\.New\(|zap\.L\(\)|zap\.S\(\)|zap\.ReplaceGlobals\(|zapcore\.NewCore\(|slog\.|log\.(Print|Fatal|Panic|Printf)|prometheus\.DefaultRegisterer|prometheus\.MustRegister\(|prometheus/promauto|otel\.SetTracerProvider|sdktrace\.NewTracerProvider|sdktrace\.NewBatchSpanProcessor\(|promhttp\.Handler\(\)|InitTracingFromEnv\(\)' \
  --include='*.go' \
  --exclude='*_test.go' \
  . 2>/dev/null | grep -v '/docs/swagger/' || true)

if [ -n "$offenders" ]; then
	echo "FAIL: observability bypass of platform-gincommon:"
	echo "  logs    → use logger.NewLogger / port.Logger / pgadapter.NewLoggerAdapter"
	echo "  metrics → use metrics.Register / gincommon.MetricsRegisterer / gincommon.MetricsHandler"
	echo "  traces  → use gincommon.InitTracingWithConfig / gincommon.NewSpanTracer"
	echo ""
	echo "$offenders"
	exit 1
fi
echo "OK: no slog/stdlib-log/direct-zap/DefaultRegisterer/bare promhttp/hand-rolled TracerProvider in production code"
