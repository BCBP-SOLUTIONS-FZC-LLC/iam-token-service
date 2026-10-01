# Observability registry proposals

This document is `iam-token-service`'s submission package for the
Enterprise Platform Observability Standard's **Registry Ratification
Requirement**: any `platform_*` metric not already canonical in the
Platform Observability Registry, and any `<domain>_*` metric proposed for
a Domain Metric Registry, must be submitted with a semantic definition,
required labels, allowed label values, and aggregation expectations before
broad adoption.

This service has **no direct channel to an external registry-governance
body** — there is no platform-wide observability registry repository this
service can open a PR against from within its own repo. This document is
the concrete, reviewable artifact a human governance reviewer needs to
ratify or reject each proposal; until that happens, treat every metric
below as **proposed, not ratified**, and the legacy `iam_token_service_*`
metric each one parallels as the still-authoritative source for
dashboards/alerts/SLOs.

All three metrics are implemented and dual-emitted today (see
`internal/adapter/outbound/metrics/metrics.go`) alongside their legacy
Tier-3 equivalent, per the Standard's Backward Compatibility migration
process (step 1: emit old and new in parallel).

**Update (platform libraries' metrics contract):** Proposals 1 and 2 are
now entries in the central Platform Observability Registry
(platform-gincommon `internal/obsregistry/registry.json`, status
`proposed`, owner platform-events) with platform-events' label shapes —
`platform_dependency_request_seconds{dependency, operation, outcome}` and
`platform_duplicate_messages_total{queue, event_type}`. This service now
emits exactly those shapes and records into platform-events' own
collectors (one series set per process; see the metrics package doc), so
the label tables below reflect the registry. Proposal 3 is not in the
registry yet.

---

## Proposal 1 — `platform_dependency_request_seconds` (Tier 1)

**Status:** registry-proposed (explicitly named as a candidate in the
Standard itself). **Parallels:** `iam_token_service_openbao_call_duration_seconds`.

### Semantic definition

Latency of one outbound call this service makes to a named external
dependency it does not own — i.e. the request/response round-trip time
observed from the calling service's side, inclusive of network time,
irrespective of whether the call ultimately succeeded or failed (an
unsuccessful call still occupied a connection and consumed time, and its
latency is exactly as operationally relevant as a successful one). This is
a **histogram** (Standard rule #5 — histograms end in `_seconds`).

### Required labels

| Label | Meaning | Source |
|---|---|---|
| `domain` | Owning platform domain | Centrally injected (`"iam"`) |
| `service` | Emitting service (`APP_NAME`) | Centrally injected by platform-gincommon (`"iam-token-service"`) — the same value platform-events and platform-pgcommon emit |
| `environment` | Deployment environment | Centrally injected from `APP_ENV` |

### Approved dimensions used

| Label | Allowed values (this service) | Meaning |
|---|---|---|
| `dependency` | `openbao` | The external system being called (platform-events adds `sns`, `sqs`, `codec` in the same process) |
| `operation` | `write`, `delete` | The logical operation performed against that dependency |
| `outcome` | `success`, `error` | Whether the call returned an error (registry outcome vocabulary) |

`read`/`list` OpenBao calls are deliberately NOT instrumented on this
metric (nor on its legacy parallel) — TS-1's rotation_id-replay `Read` is
rare and outside the frozen op set, and `List` is the orphan-material
reconciler's own enumeration, already covered by
`iam_token_service_material_reconcile_total`. A future service adding a new
dependency (e.g. a second downstream API) would add a new `dependency`
value here, not a new metric.

### Aggregation expectations

- `sum by (dependency) (rate(platform_dependency_request_seconds_sum{domain="iam"}[5m])) / sum by (dependency) (rate(platform_dependency_request_seconds_count{domain="iam"}[5m]))` — average per-dependency latency across every IAM service, once other IAM services adopt this metric.
- `histogram_quantile(0.99, sum by (le, dependency, service) (rate(platform_dependency_request_seconds_bucket{domain="iam"}[5m])))` — p99 latency per dependency per service.
- Cross-domain rollup (once Billing/Workflow/etc. adopt it): `sum by (domain, dependency) (rate(platform_dependency_request_seconds_sum[5m]))`.

---

## Proposal 2 — `platform_duplicate_messages_total` (Tier 1)

**Status:** registry-proposed (explicitly named as a candidate in the
Standard itself). **Parallels:** `iam_token_service_processed_events_duplicates_total`.

### Semantic definition

Count of inbound messages a consumer recognized as a redelivery (already
present in its idempotency ledger) and skipped without reprocessing. This
is a **counter** (Standard rule #4 — counters end in `_total`) and MUST
only increment on an actual detected duplicate, never on a first-time
delivery.

### Required labels

| Label | Meaning | Source |
|---|---|---|
| `domain` | Owning platform domain | Centrally injected (`"iam"`) |
| `service` | Emitting service (`APP_NAME`) | Centrally injected by platform-gincommon (`"iam-token-service"`) — the same value platform-events and platform-pgcommon emit |
| `environment` | Deployment environment | Centrally injected from `APP_ENV` |

### Approved dimensions used

| Label | Allowed values (this service) | Meaning |
|---|---|---|
| `queue` | `tenant-lifecycle-tokensvc-q` | The SQS queue the duplicate was received on |
| `event_type` | `TenantMembershipsPurged` | The duplicate's envelope type (registry shape, shared with platform-events' inbox) |

This service has exactly one inbound subscription today
(`TenantMembershipsPurged` on `tenant-lifecycle-tokensvc-q`, §7.1), so
`queue` has exactly one value in practice; the label exists so a
multi-queue consumer (this service's own future state, or any other
service) differentiates without a new metric name.

### Aggregation expectations

- `sum by (queue) (rate(platform_duplicate_messages_total{domain="iam"}[1h]))` — duplicate-delivery rate per queue, an SQS at-least-once health signal (a sustained high rate suggests a downstream ack/visibility-timeout problem, not necessarily a bug in this consumer).
- Cross-domain rollup: `sum by (domain) (rate(platform_duplicate_messages_total[1h]))` once other domains adopt it — a platform-wide view of redelivery pressure.

---

## Proposal 3 — `iam_offboarding_cascade_total` (Tier 2, IAM Domain Metric Registry)

**Status:** proposed for the IAM domain (not a Tier-1 candidate — this
concept is IAM-specific, not applicable to Billing/Workflow/etc.).
**Parallels:** `iam_token_service_offboarding_cascade_total`.

**Not emitted until ratified.** Under the platform libraries' metrics
contract an `iam_*` name outside `iam_token_service_*` must be an entry in
the central Platform Observability Registry before any service emits it
(`metricslint check` reports it as an error otherwise). The service stopped
dual-emitting it; `iam_token_service_offboarding_cascade_total` is the only
cascade metric until this proposal is added to the registry. Note for the
reviewer: the registry's `outcome` vocabulary is `success | failure |
error`, so `ok` below should become `success` on ratification.

### Semantic definition

Outcome of one IAM service's own reaction to a tenant-offboarding signal
(this service's `TenantMembershipsPurged` consumer) — i.e. "did this
service's own domain-data cleanup for this tenant succeed or fail,"
independent of what any other IAM service's cleanup did. Every IAM service
that consumes a tenant-offboarding event and cascades its own cleanup
(org-membership's membership rows, user-profile's profile rows, this
service's credential/OpenBao-material rows, ...) can emit this metric with
identical semantics — the Standard's own worked example
(`iam_auth_events_total`) is exactly this kind of "same shape, different
service" domain concept.

This is a **counter**.

### Required labels

| Label | Meaning | Source |
|---|---|---|
| `service` | Emitting service (`APP_NAME`) | Centrally injected by platform-gincommon (`"iam-token-service"`) — the same value platform-events and platform-pgcommon emit |
| `environment` | Deployment environment | Centrally injected from `APP_ENV` |

`domain` is not a label — it is already the `iam_` name prefix, per the
Standard's own `iam_auth_events_total{service="event-consumer",
environment="prod"}` example (no `domain` label shown).

### Approved dimension used

| Label | Allowed values | Meaning |
|---|---|---|
| `outcome` | `ok`, `error` | Whether this service's own cascade completed without error |

Chosen from the Standard's own listed IAM domain label examples
(`event_type`, `outcome`, `strategy`, `source`) rather than reusing this
service's legacy `result` label name, so the label vocabulary is
consistent the moment a second IAM service adopts this metric.

### Aggregation expectations

- `sum by (service) (rate(iam_offboarding_cascade_total{outcome="error"}[1h]))` — per-service cascade failure rate, once every IAM service that reacts to tenant offboarding adopts this name.
- `sum(rate(iam_offboarding_cascade_total{outcome="error"}[1h])) > 0` — a single domain-wide alert catching ANY IAM service's offboarding-cascade failure, not just this one's (not currently wired as an alert — see Migration status below).

---

## Migration status

| Legacy (Tier 3) | Proposed | Dual-emitted? | Alerts migrated? | Dashboards migrated? |
|---|---|---|---|---|
| `iam_token_service_openbao_call_duration_seconds` | `platform_dependency_request_seconds` | Yes | No — `IAMTokenServiceOpenBaoCallLatencyHigh` (`deploy/monitoring/app-alerts.yml`) still reads the legacy metric until ratified | No — out of this repo's scope; no cross-service dashboard repo is available to this service |
| `iam_token_service_processed_events_duplicates_total` | `platform_duplicate_messages_total` | Yes | No | No |
| `iam_token_service_offboarding_cascade_total` | `iam_offboarding_cascade_total` | No — withheld until ratified in the registry | No | No |

Per the Standard's Backward Compatibility process, steps 2–8 (migrate
dashboards → migrate alerts → migrate recording rules → migrate SLOs →
migrate HPA references → deprecate legacy → remove after sunset) are
deliberately **not** started for any of the three proposals — they only
make sense to start once a governance reviewer has actually ratified the
name, labels, and semantics documented above. Removing
`iam_token_service_openbao_call_duration_seconds` (etc.) before that would
break the existing alert with nothing to replace it.
