# Renamed and removed metric names

iam-token-service moved to the Enterprise Platform Observability Standard in two steps. The service has never been deployed to any environment, so no dashboard, alert or stored series depended on the old names. Library names were **removed outright, with no compatibility period**, and the alerts, recording rules, SLOs, HPA rule and docs in this repo were rewritten in the same change. `metricslint refs` (in CI, `make metrics-lint`) rejects any removed `platform_*` or library name that comes back.

The service's own `iam_token_service_*` names are frozen by LLD §25 and were **not** renamed. They are already Tier 3 (`<domain>_<service>_<metric>`).

## 1. Three-tier adoption (additive)

The standard's tiers were adopted without renaming anything. Three registry candidates were dual-emitted next to the Tier 3 metric they parallel ([`../observability-registry-proposals.md`](../observability-registry-proposals.md)):

| Tier 3 (kept, authoritative) | Candidate | Status today |
|---|---|---|
| `iam_token_service_openbao_call_duration_seconds{op}` | `platform_dependency_request_seconds{dependency="openbao", operation, outcome}` | Dual-emitted; shape corrected in step 2 |
| `iam_token_service_processed_events_duplicates_total{consumer}` | `platform_duplicate_messages_total{queue, event_type}` | Dual-emitted; shape corrected in step 2 |
| `iam_token_service_offboarding_cascade_total{result}` | `iam_offboarding_cascade_total{outcome}` | **No longer emitted** (step 2): not a registry entry |

## 2. Platform library migration (2026-10-02)

The platform libraries dropped their pre-standard names (platform-gincommon v1.4.0, platform-events/v2 v2.0.0, platform-pgcommon/v2 v2.0.0). Every query in this repo was rewritten to the `platform_*` successor:

| Removed name | Replacement | Label changes |
|---|---|---|
| `http_requests_total` | `platform_http_requests_total` | + `domain`, `environment`; − `version`; `status` → `status_class` (the 5xx queries now select `status_class="5xx"`) |
| `http_request_duration_seconds` | `platform_http_request_duration_seconds` | same; identical buckets; the TS-1 latency SLI selects `route` instead of `path` |
| `http_requests_per_second` (prometheus-adapter / HPA) | `platform_http_requests_per_second` | derived from `platform_http_requests_total` |
| `outbox_dead_letters_total` | `platform_dlq_messages_total{operation="outbox_publish", reason="max_attempts"}` | |
| `outbox_pending_total` | `platform_outbox_pending_events` | never `-1`; a failed count keeps the last value and increments `platform_outbox_errors_total{operation="pending_count"}` |
| `sqs_receive_errors_total` | `platform_dependency_request_seconds_count{dependency="sqs", operation="receive_message", outcome="error"}` | no `queue` label (this service consumes one queue) |
| `pgcommon_pool_empty_acquire_total` (and the other `pgcommon_*` names) | `platform_db_pool_empty_acquires_total` (`platform_db_*`) | `pool` label: `default` (app pool) and `reconciler` (BYPASSRLS pool), replacing the `-reconciler` suffix on `service` |

Corrections made in the same change:

| Before | After | Why |
|---|---|---|
| `platform_dependency_request_seconds{dependency, operation}` defined by this service | registry shape `{dependency, operation, outcome}`, recorded into platform-events' own collector (`registerShared`) | The old shape made platform-events' SNS/SQS/codec series fail to register |
| `platform_duplicate_messages_total{queue}` | `{queue, event_type}` | Registry shape, shared with platform-events' inbox |
| `service="token-service"` on Tier 1/2 series | `service="iam-token-service"` (gincommon's identity) | One `service` value across every library in the same scrape |
| `iam_offboarding_cascade_total` dual-emitted | not emitted | An `iam_*` name outside `iam_token_service_*` must be a registry entry first |
| Helm `appEnv: production` | `appEnv: prod` | platform-gincommon rejects anything outside `local, dev, test, staging, prod` |

The identity became mandatory at the same time: `domain="iam"` via `gincommon.Config.Domain` / `OBSERVABILITY_DOMAIN` (Helm `observabilityDomain`). platform-events' `events_*` / `outbox_*` / `sqs_*` and platform-pgcommon's `pgcommon_*` replacements are listed in each library's `docs/observability/`.

## 3. platform-gincommon v1.6.0 alignment (2026-10-05)

No metric names changed. Wiring brought in line with iam-org-membership:

| Before | After | Effect on series |
|---|---|---|
| `TimeoutMiddleware(30s)` as an outer `r.Use` (server, consumer health router) | `gincommon.Config.RequestTimeout = 30s` | A timed-out request is now recorded as 503 on `platform_http_requests_total` / `platform_http_request_timeouts_total`; it was recorded as 200 before, while the client got 503 |
| `promhttp.Handler()` | `gincommon.MetricsHandler()` | Same registry; one failing collector no longer blanks the whole scrape |
| `InitTracingFromEnv()` | `InitTracingWithConfig` with the metrics identity | Traces carry `service.namespace="iam"` and `deployment.environment.name` |

## 4. Observability gaps closed (2026-10-05)

| Before | After | Why |
|---|---|---|
| `IAMTokenServiceRLSCrossTenantAccess` / `IAMTokenServiceRLSMissingGUC` on `iam_token_service_rls_violations_total` | `iam_rls_violations_total{violation_type}` (Tier 2, registry entry) | The old name was never emitted, so both alerts could not fire. The registry's shared IAM name is used instead of a new Tier 3 one (TS-D18) |
| `platform_queue_depth` / `platform_dlq_depth` not emitted | sampled every `SQS_QUEUE_DEPTH_INTERVAL` (60s) by `cmd/consumer` | New alerts `IAMTokenServiceOffboardingQueueStalled` and `IAMTokenServiceOffboardingDLQBacklog`; the CloudWatch-based age alert stays |
| Hand-rolled `processed_events` dedup (check, then record in a later transaction); the service recorded `platform_duplicate_messages_total` itself | platform-events' `pkg/inbox` (claim + cascade in one transaction, TS-D19); the inbox records `platform_duplicate_messages_total`, the service only the Tier 3 `iam_token_service_processed_events_duplicates_total` | Same as iam-org-membership; one counter per library, no double counting |
| SLO targets marked unreviewed placeholders | SLO-1, SLO-2 and the new SLO-4 (TS-3 read latency) cite LLD §11; SLO-3 is labelled an operational proxy | The LLD already set the targets |

## Querying after the migration

```promql
# 5xx ratio (was http_requests_total{status=~"5.."})
sum by (environment) (rate(platform_http_requests_total{service="iam-token-service",status_class="5xx"}[5m]))
  / sum by (environment) (rate(platform_http_requests_total{service="iam-token-service"}[5m]))

# Outbox dead letters (was outbox_dead_letters_total)
sum by (environment) (increase(platform_dlq_messages_total{service="iam-token-service",operation="outbox_publish"}[30m]))

# OpenBao p99 — Tier 3 stays authoritative until the candidate is ratified
histogram_quantile(0.99, sum by (le, op) (rate(iam_token_service_openbao_call_duration_seconds_bucket[5m])))
```
