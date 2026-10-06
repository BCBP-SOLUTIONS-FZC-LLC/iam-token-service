#!/usr/bin/env bash
# Observability must enter the process only through platform-gincommon,
# matching iam-user-profile / iam-org-membership (every binary:
# cmd/server, cmd/consumer, cmd/rotator, cmd/scheduler):
#   logs    — Zap via logger.NewLogger (port.Logger), never slog / stdlib log / zap.New
#   metrics — gincommon.MetricsRegisterer / MetricsHandler, never prometheus.DefaultRegisterer /
#             promauto / a bare promhttp.Handler
#   traces  — gincommon.InitTracingWithConfig (same identity as metrics), never
#             InitTracingFromEnv or a hand-rolled TracerProvider
set -euo pipefail

echo "Checking logs/metrics/traces go through platform-gincommon (Zap)..."

offenders=$(grep -rnE \
  'log/slog|"log"|go\.uber\.org/zap|zap\.New\(|slog\.|log\.(Print|Fatal|Panic|Printf)|prometheus\.DefaultRegisterer|prometheus/promauto|otel\.SetTracerProvider|sdktrace\.NewTracerProvider|promhttp\.Handler\(\)|InitTracingFromEnv\(\)' \
  --include='*.go' \
  --exclude='*_test.go' \
  . 2>/dev/null | grep -v '/docs/swagger/' || true)

if [ -n "$offenders" ]; then
	echo "FAIL: observability bypass of platform-gincommon (use logger.NewLogger / MetricsRegisterer / MetricsHandler / InitTracingWithConfig):"
	echo "$offenders"
	exit 1
fi
echo "OK: no slog/stdlib-log/direct-zap/DefaultRegisterer/bare promhttp/hand-rolled TracerProvider in production code"
