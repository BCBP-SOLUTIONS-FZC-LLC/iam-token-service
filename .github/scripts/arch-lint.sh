#!/usr/bin/env bash
# Installs (if needed) and runs go-arch-lint against .go-arch-lint.yml,
# enforcing this service's Clean Architecture layering: core/domain ->
# core/port -> service/postgres/openbao/eventbus/metrics ->
# adapters_inbound/adapters_outbound/reconciler_jobs -> cmd (§3.4/§6).
# reconciler_jobs (cmd/rotator) may not depend on service; cmd/scheduler may.
#
# TS-INV-1 (no Keycloak), TS-INV-2 (no secret in logs), and gincommon
# observability (Zap / MetricsRegisterer / InitTracingWithConfig) have their
# own dedicated gates, wired via `make gates` — not duplicated here.
set -euo pipefail
if ! command -v go-arch-lint >/dev/null; then
  go install github.com/fe3dback/go-arch-lint@v1.15.0
fi
go-arch-lint check --project-path .
