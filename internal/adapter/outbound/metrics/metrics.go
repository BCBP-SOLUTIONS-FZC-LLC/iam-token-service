// Package metrics registers this service's Prometheus instruments per the
// Enterprise Platform Observability Standard's three-tier taxonomy:
//
//   - Tier 1 (platform_*) — semantics shared across every domain
//     (IAM/Workflow/Billing/...). Required labels: domain, service,
//     environment. Any platform_* name not already canonical is a
//     REGISTRY-PROPOSED metric — see docs/observability-registry-proposals.md
//     for the formal submission (semantic definition, labels, allowed
//     values, aggregation expectations) this package's proposed metrics
//     require before they may be treated as ratified.
//   - Tier 2 (iam_*) — semantics shared across IAM services, not meaningful
//     outside the domain. Required labels: service, environment.
//   - Tier 3 (iam_token_service_*, frozen §25) — this service only. No
//     cross-service label contract; kept exactly as the LLD froze them.
//
// Migration posture (per the Standard's Backward Compatibility section):
// every Tier-1/Tier-2 metric below is emitted IN PARALLEL with its
// pre-existing Tier-3 equivalent, not as a replacement — dashboards/alerts
// keep working against the legacy name during the compatibility period,
// and only migrate + the legacy metric is removed after governance
// ratifies the new name and a sunset period elapses (see CHANGELOG.md).
package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

const (
	tier3Prefix = "iam_token_service_" // frozen, §25 — Tier 3 naming: <domain>_<service>_<metric>.
	tier2Prefix = "iam_"               // Tier 2 naming: <domain>_<metric>.
	tier1Prefix = "platform_"          // Tier 1 naming: platform_<metric>.

	// domainName and serviceName are this package's own copies of the
	// Tier-1/Tier-2 `domain`/`service` label values — deliberately NOT
	// reusing gincommon's {service} const label (which is
	// "iam-token-service", the pre-Standard domain+service-combined name):
	// the Standard's `service` label is domain-less ("token-service") with
	// `domain` carried as its own separate label, so a Tier-1/2 consumer
	// can group by domain without string-splitting a label value.
	domainName  = "iam"
	serviceName = "token-service"
)

var (
	// ── Tier 3: iam_token_service_* (frozen §25, service-owned) ──────────

	// CredentialsIssuedTotal counts credential state transitions by op
	// (issue|rotate|revoke). Incremented only on a REAL transition — a
	// rotation_id replay (§9.2, no new material/row/event) does not count.
	// Service-specific: no other IAM service issues this kind of
	// machine credential, so this stays Tier 3 (Standard §"Namespace
	// Selection Rules" step 3 — not reusable outside this service).
	CredentialsIssuedTotal *prometheus.CounterVec

	// RotationOverlapActive is the current count of `rotating` credentials
	// across all tenants — should trend to zero between rotations.
	// Refreshed periodically from the BYPASSRLS reconciler pool (§4.3),
	// the same cross-tenant-enumeration pattern the §8.3/§8.6 jobs use.
	// Service-specific: the rotation-overlap concept only exists in this
	// service's credential lifecycle model.
	RotationOverlapActive prometheus.Gauge

	// OpenBaoCallDuration is OpenBao KV v2 call latency by op (write|delete).
	// Legacy Tier-3 name kept for the compatibility period — see
	// DependencyRequestDuration (Tier 1, dual-emitted alongside this).
	OpenBaoCallDuration *prometheus.HistogramVec

	// OffboardingCascadeTotal counts offboarding-cascade outcomes
	// (§8.4) by result (ok|error). Legacy Tier-3 name kept for the
	// compatibility period — see IAMOffboardingCascadeTotal (Tier 2,
	// dual-emitted alongside this).
	OffboardingCascadeTotal *prometheus.CounterVec

	// RotationSweepTotal counts overlap-expiry sweep outcomes (§8.3) by
	// result (ok|error). Service-specific: no other IAM service runs a
	// credential-rotation sweep.
	RotationSweepTotal *prometheus.CounterVec

	// MaterialReconcileTotal counts orphaned-material reconciler outcomes
	// (§8.6) by result (orphan_deleted|missing_material|ok|error).
	// missing_material is page-worthy (§11.5). Service-specific: OpenBao
	// credential-material reconciliation is unique to this service.
	MaterialReconcileTotal *prometheus.CounterVec

	// CadenceRotationTotal counts cmd/scheduler's automatic cadence-driven
	// rotation outcomes (§16 TSQ-6 Resolved, TS-D14) by result
	// (rotated|skipped|failed). failed is page-worthy (§11.5): it may mean
	// IssueOrRotate committed but the RP-17 relay failed, leaving Keycloak
	// out of sync with this service's own record (cmd/scheduler/scan.go).
	// Service-specific: automatic cadence-driven rotation is unique to
	// this service.
	CadenceRotationTotal *prometheus.CounterVec

	// JWKSKeyErrorsTotal counts live (active/rotating) credentials the
	// JWKS route (§5.4, EXT-6) could not serve — its OpenBao material was
	// unreadable or unparseable (production-readiness review, TS-D15).
	// Page-worthy (§11.5): unlike an unknown tenant (an intentionally
	// unsignaled 200 with zero keys), this is a credential this service
	// itself believes is live, so >0 means a real per-credential Keycloak
	// auth outage masquerading as a 200. No `result` label (unlike the
	// counters above) — there is exactly one outcome this counts.
	// Service-specific: JWKS custody is unique to this service.
	JWKSKeyErrorsTotal prometheus.Counter

	// ProcessedEventsDuplicates counts SQS redeliveries filtered by the
	// processed_events composite PK, matching iam-user-profile /
	// iam-org-membership. Legacy Tier-3 name kept for the compatibility
	// period — see DuplicateMessagesTotal (Tier 1, dual-emitted alongside
	// this).
	ProcessedEventsDuplicates *prometheus.CounterVec

	// UnknownEventAcknowledged counts inbound events with no registered
	// handler — acknowledged into processed_events so redelivery does not
	// DLQ-storm on a producer schema addition. Service-specific per the
	// Standard's own Tier-3 worked example (iam_event_consumer_unknown_event_acknowledged_total)
	// — this is exactly that pattern, just for this service's consumer.
	UnknownEventAcknowledged *prometheus.CounterVec

	// ── Tier 1: platform_* (REGISTRY-PROPOSED — see package doc) ─────────

	// DependencyRequestDuration is the proposed platform_dependency_request_seconds:
	// latency of an outbound call to a named external dependency, labeled
	// {domain, service, environment, dependency, operation}. This
	// package's only current dependency label value is "openbao"
	// (write|delete operations) — dual-emitted alongside the legacy
	// OpenBaoCallDuration.
	DependencyRequestDuration *prometheus.HistogramVec

	// DuplicateMessagesTotal is the proposed platform_duplicate_messages_total:
	// inbound messages recognized as redeliveries and skipped, labeled
	// {domain, service, environment, queue}. Dual-emitted alongside the
	// legacy ProcessedEventsDuplicates.
	DuplicateMessagesTotal *prometheus.CounterVec

	// ── Tier 2: iam_* (proposed for the IAM Domain Metric Registry) ──────

	// IAMOffboardingCascadeTotal is the proposed iam_offboarding_cascade_total:
	// outcomes of a service's own tenant-offboarding cascade, labeled
	// {service, environment, outcome} — semantically identical wherever an
	// IAM service reacts to TenantMembershipsPurged by cleaning up its own
	// domain data (this service's credential/OpenBao-material cleanup,
	// org-membership's membership-row cleanup, etc. — Standard §"Domain
	// metrics must have identical semantic definitions across all services
	// within the domain"). Dual-emitted alongside the legacy
	// OffboardingCascadeTotal.
	IAMOffboardingCascadeTotal *prometheus.CounterVec
)

var (
	registerOnce sync.Once
	environment  string // set once by Register; read-only thereafter.
)

// gincommonLabels returns a copy of gincommon's {service, version} const
// labels so Tier-3 collectors scrape on the same registry and labels as
// HTTP metrics. Returns nil when ObservabilityMiddlewares has not run yet,
// matching prometheus's "no const labels" zero value so unit tests that
// never bootstrap gincommon still register cleanly.
func gincommonLabels() prometheus.Labels {
	labels := gincommon.MetricsConstLabels()
	if len(labels) == 0 {
		return nil
	}
	return labels
}

// tier3Labels merges gincommon's {service, version} with `environment` —
// the const-label set for this service's own iam_token_service_* metrics.
func tier3Labels() prometheus.Labels {
	return withEnvironment(gincommonLabels())
}

// tier2Labels is the Standard's Tier-2 required label set: {service,
// environment} — domain is not repeated as a label because it is already
// the iam_ name prefix. `service` here is deliberately the domain-less
// name ("token-service"), per this package's doc comment.
func tier2Labels() prometheus.Labels {
	return prometheus.Labels{"service": serviceName, "environment": environment}
}

// tier1Labels is the Standard's Tier-1 required label set: {domain,
// service, environment}.
func tier1Labels() prometheus.Labels {
	return prometheus.Labels{"domain": domainName, "service": serviceName, "environment": environment}
}

// withEnvironment returns a copy of base with "environment" added — nil
// base still yields a valid non-nil map (environment is always known by
// the time Register runs, unlike gincommon's labels which may be empty in
// tests).
func withEnvironment(base prometheus.Labels) prometheus.Labels {
	out := make(prometheus.Labels, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["environment"] = environment
	return out
}

// Register wires this service's business metrics onto gincommon's
// Prometheus registerer (same registry as HTTP metrics). Call once at
// startup AFTER ObservabilityMiddlewares has run and BEFORE /metrics is
// served, passing the resolved APP_ENV value (the Standard's centrally-
// injected `environment` label — instrumentation call sites never set it
// themselves, per requirement #8: labels must be injected centrally so
// they cannot be omitted or misspelled). Idempotent. Every one of
// cmd/server, cmd/consumer, cmd/rotator, and cmd/scheduler calls this —
// the instruments used by a given binary are simply the ones that
// binary's code paths touch; registering the full set everywhere keeps
// one source of truth instead of four partial ones.
func Register(env string) {
	registerOnce.Do(func() {
		environment = env
		registerMetrics()
	})
}

func registerMetrics() {
	t3 := tier3Labels()
	t2 := tier2Labels()
	t1 := tier1Labels()

	// ── Tier 3 ────────────────────────────────────────────────────────
	CredentialsIssuedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "credentials_issued_total",
		Help:        "Credential state transitions by op (issue|rotate|revoke).",
		ConstLabels: t3,
	}, []string{"op"})

	RotationOverlapActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        tier3Prefix + "rotation_overlap_active",
		Help:        "Current count of `rotating` credentials across all tenants — should trend to zero between rotations.",
		ConstLabels: t3,
	})

	OpenBaoCallDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        tier3Prefix + "openbao_call_duration_seconds",
		Help:        "[Legacy — see platform_dependency_request_seconds{dependency=\"openbao\"}] OpenBao KV v2 call latency by op (write|delete).",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: t3,
	}, []string{"op"})

	OffboardingCascadeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "offboarding_cascade_total",
		Help:        "[Legacy — see iam_offboarding_cascade_total] Offboarding-cascade outcomes (§8.4) by result (ok|error).",
		ConstLabels: t3,
	}, []string{"result"})

	RotationSweepTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "rotation_sweep_total",
		Help:        "Overlap-expiry sweep outcomes (§8.3) by result (ok|error).",
		ConstLabels: t3,
	}, []string{"result"})

	MaterialReconcileTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "material_reconcile_total",
		Help:        "Orphaned-material reconciler outcomes (§8.6) by result (orphan_deleted|missing_material|ok|error). missing_material is page-worthy.",
		ConstLabels: t3,
	}, []string{"result"})

	CadenceRotationTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "cadence_rotation_total",
		Help:        "cmd/scheduler's automatic cadence-driven rotation outcomes (§16 TSQ-6 Resolved) by result (rotated|skipped|failed). failed is page-worthy.",
		ConstLabels: t3,
	}, []string{"result"})

	ProcessedEventsDuplicates = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "processed_events_duplicates_total",
		Help:        "[Legacy — see platform_duplicate_messages_total] Inbound SQS messages skipped because event_id was already in processed_events.",
		ConstLabels: t3,
	}, []string{"consumer"})

	UnknownEventAcknowledged = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "unknown_event_acknowledged_total",
		Help:        "Inbound events with no handler registered — acknowledged without processing.",
		ConstLabels: t3,
	}, []string{"consumer", "event_type"})

	// ── Tier 1 (registry-proposed) ───────────────────────────────────────
	DependencyRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        tier1Prefix + "dependency_request_seconds",
		Help:        "[REGISTRY-PROPOSED, pending Platform Observability Registry ratification — see docs/observability-registry-proposals.md] Outbound dependency call latency by dependency and operation.",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: t1,
	}, []string{"dependency", "operation"})

	DuplicateMessagesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier1Prefix + "duplicate_messages_total",
		Help:        "[REGISTRY-PROPOSED, pending Platform Observability Registry ratification — see docs/observability-registry-proposals.md] Inbound messages recognized as redeliveries and skipped, by queue.",
		ConstLabels: t1,
	}, []string{"queue"})

	// ── Tier 2 (proposed for the IAM Domain Metric Registry) ─────────────
	IAMOffboardingCascadeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier2Prefix + "offboarding_cascade_total",
		Help:        "[PROPOSED for the IAM Domain Metric Registry — see docs/observability-registry-proposals.md] Per-service tenant-offboarding cascade outcomes by outcome (ok|error).",
		ConstLabels: t2,
	}, []string{"outcome"})

	JWKSKeyErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        tier3Prefix + "jwks_key_errors_total",
		Help:        "Live credentials the JWKS route (EXT-6) could not serve — OpenBao material unreadable/unparseable. Page-worthy: a real per-credential Keycloak auth outage.",
		ConstLabels: t3,
	})

	gincommon.MetricsRegisterer().MustRegister(
		CredentialsIssuedTotal,
		RotationOverlapActive,
		OpenBaoCallDuration,
		OffboardingCascadeTotal,
		RotationSweepTotal,
		MaterialReconcileTotal,
		// CadenceRotationTotal was defined but never registered until this
		// production-readiness review (TS-D15) — iam_token_service_
		// cadence_rotation_total never actually reached /metrics, so the
		// IAMTokenServiceCadenceRotationFailures alert added alongside it
		// could never have fired.
		CadenceRotationTotal,
		JWKSKeyErrorsTotal,
		ProcessedEventsDuplicates,
		UnknownEventAcknowledged,
		DependencyRequestDuration,
		DuplicateMessagesTotal,
		IAMOffboardingCascadeTotal,
	)

	// Pre-initialize labels so dashboards show 0 rather than "no data".
	for _, op := range []string{"issue", "rotate", "revoke"} {
		CredentialsIssuedTotal.WithLabelValues(op)
	}
	for _, op := range []string{"write", "delete"} {
		OpenBaoCallDuration.WithLabelValues(op)
		DependencyRequestDuration.WithLabelValues("openbao", op)
	}
	for _, result := range []string{"ok", "error"} {
		OffboardingCascadeTotal.WithLabelValues(result)
		IAMOffboardingCascadeTotal.WithLabelValues(result)
		RotationSweepTotal.WithLabelValues(result)
	}
	for _, result := range []string{"orphan_deleted", "missing_material", "ok", "error"} {
		MaterialReconcileTotal.WithLabelValues(result)
	}
	ProcessedEventsDuplicates.WithLabelValues("tenant_offboarding")
	DuplicateMessagesTotal.WithLabelValues("tenant-lifecycle-tokensvc-q")
}
