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
//     outside the domain. Required labels: service, environment. An iam_*
//     name must be a ratified Platform Observability Registry entry before
//     it is emitted, so this package emits none today (the proposed
//     iam_offboarding_cascade_total waits on ratification).
//   - Tier 3 (iam_token_service_*, frozen §25) — this service only. No
//     cross-service label contract; kept exactly as the LLD froze them.
//
// Every collector carries gincommon's centrally injected {domain, service,
// environment} const labels (gincommon.MetricsConstLabels) — the same values
// platform-gincommon, platform-events and platform-pgcommon put on their
// platform_* series, so one scrape joins cleanly on them.
//
// The two Tier 1 names this service records into
// (platform_dependency_request_seconds, platform_duplicate_messages_total)
// are owned by platform-events, which registers them with the registry
// shape in InitLibraryMetrics. Register therefore reuses platform-events'
// collector (same descriptor ⇒ prometheus.AlreadyRegisteredError) instead
// of registering a second, differently-shaped one — which would disable
// platform-events' own SNS/SQS/codec series (fail-soft RegistrationWarning).
//
// The service's own Tier-3 names (iam_token_service_*) predate the platform
// libraries' migration and are unchanged; each Tier-1 metric below is still
// emitted in parallel with its Tier-3 equivalent until governance ratifies
// the new name (see CHANGELOG.md).
package metrics

import (
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/obsregistry"
)

const (
	tier3Prefix = "iam_token_service_" // frozen, §25 — Tier 3 naming: <domain>_<service>_<metric>.

	// domainName and serviceName are the identity fallbacks used only
	// before gincommon.ObservabilityMiddlewares has run (unit tests that
	// never bootstrap gincommon). In every binary the labels come from
	// gincommon.MetricsConstLabels, so this package's `service` value is
	// exactly the one gincommon, platform-events and platform-pgcommon emit
	// (APP_NAME, "iam-token-service").
	domainName  = ObservabilityDomain
	serviceName = "iam-token-service"

	// Tier 1 names owned by platform-events (see package doc).
	dependencyRequestSecondsName = "platform_dependency_request_seconds"
	duplicateMessagesTotalName   = "platform_duplicate_messages_total"
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
	// (§8.4) by result (ok|error). Authoritative: the proposed Tier-2
	// iam_offboarding_cascade_total (docs/observability-registry-proposals.md,
	// Proposal 3) is not emitted until it is ratified in the Platform
	// Observability Registry — the platform libraries' contract rejects an
	// unregistered iam_* name (only iam_token_service_* is service-owned).
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

	// ConsumedSchemaViolationsTotal counts inbound events whose payload
	// failed this service's embedded consumed-event schema
	// (cmd/consumer/inbound_schema.go) and were sent straight to the
	// queue's -dlq (cmd/consumer/dlq.go) without reaching the offboarding
	// cascade or processed_events. Any nonzero rate means the producer
	// (iam-org-membership) broke its contract or api/asyncapi.yaml is stale
	// — the tenant's service accounts are not being cleaned up. Pages
	// (IAMTokenServiceConsumedSchemaViolation). Tier 3, authoritative;
	// bounded by this service's one consumer and its one consumed type.
	ConsumedSchemaViolationsTotal *prometheus.CounterVec

	// ── Tier 1: platform_* (REGISTRY-PROPOSED — see package doc) ─────────

	// DependencyRequestDuration is the registry-proposed
	// platform_dependency_request_seconds{dependency, operation, outcome}
	// (owned by platform-events, which records sns/sqs/codec into the same
	// collector). This service adds dependency="openbao", operation
	// write|delete, outcome success|error — dual-emitted alongside the
	// legacy OpenBaoCallDuration.
	DependencyRequestDuration *prometheus.HistogramVec

	// DuplicateMessagesTotal is the registry-proposed
	// platform_duplicate_messages_total{queue, event_type} (owned by
	// platform-events): inbound messages recognized as redeliveries and
	// skipped. Dual-emitted alongside the legacy ProcessedEventsDuplicates.
	DuplicateMessagesTotal *prometheus.CounterVec
)

var (
	registerOnce sync.Once
	environment  string // set once by Register; read-only thereafter.
)

// gincommonLabels returns a copy of gincommon's {domain, service,
// environment} const labels so Tier-3 collectors scrape on the same registry
// and labels as HTTP metrics. Returns nil when ObservabilityMiddlewares has not run yet,
// matching prometheus's "no const labels" zero value so unit tests that
// never bootstrap gincommon still register cleanly.
func gincommonLabels() prometheus.Labels {
	labels := gincommon.MetricsConstLabels()
	if len(labels) == 0 {
		return nil
	}
	return labels
}

// tier3Labels is gincommon's {domain, service, environment} (environment
// added explicitly so it is present even before gincommon is initialised) —
// the const-label set for this service's own iam_token_service_* metrics.
func tier3Labels() prometheus.Labels {
	return withEnvironment(gincommonLabels())
}

// tier1Labels is the Standard's Tier-1 required label set: {domain,
// service, environment} — gincommon's values (identical to
// platform-events' and platform-pgcommon's, see InitLibraryMetrics), or the
// package fallbacks before gincommon is initialised.
func tier1Labels() prometheus.Labels {
	labels := prometheus.Labels{"domain": domainName, "service": serviceName, "environment": environment}
	for k, v := range gincommonLabels() {
		if _, ok := labels[k]; ok && v != "" {
			labels[k] = v
		}
	}
	return labels
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
// startup AFTER ObservabilityMiddlewares and InitLibraryMetrics have run and
// BEFORE /metrics is served, passing the resolved APP_ENV value (gincommon's
// normalised environment label wins when it is set) (the Standard's centrally-
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
		if e := gincommonLabels()["environment"]; e != "" {
			environment = e
		}
		registerMetrics()
	})
}

func registerMetrics() {
	t3 := tier3Labels()
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
		Help:        "Offboarding-cascade outcomes (§8.4) by result (ok|error).",
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

	ConsumedSchemaViolationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "consumed_schema_violations_total",
		Help:        "Inbound events rejected straight to the -dlq because the payload failed its embedded consumed-event schema, by consumer and event type — any nonzero rate means a producer contract break.",
		ConstLabels: t3,
	}, []string{"consumer", "event_type"})

	// ── Tier 1 (registry-proposed, owned by platform-events) ─────────────
	// Built from the Platform Observability Registry entry (name, help,
	// labels, buckets) so the descriptor is identical to platform-events'
	// and registerShared can adopt its collector.
	DependencyRequestDuration = registerShared(prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        dependencyRequestSecondsName,
		Help:        registryEntry(dependencyRequestSecondsName).Help,
		Buckets:     obsregistry.Default().Buckets(registryEntry(dependencyRequestSecondsName).Buckets),
		ConstLabels: t1,
	}, registryEntry(dependencyRequestSecondsName).Labels))

	DuplicateMessagesTotal = registerShared(prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        duplicateMessagesTotalName,
		Help:        registryEntry(duplicateMessagesTotalName).Help,
		ConstLabels: t1,
	}, registryEntry(duplicateMessagesTotalName).Labels))

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
		ConsumedSchemaViolationsTotal,
	)

	// Pre-initialize labels so dashboards show 0 rather than "no data".
	for _, op := range []string{"issue", "rotate", "revoke"} {
		CredentialsIssuedTotal.WithLabelValues(op)
	}
	for _, op := range []string{"write", "delete"} {
		OpenBaoCallDuration.WithLabelValues(op)
		for _, outcome := range []string{OutcomeSuccess, OutcomeError} {
			DependencyRequestDuration.WithLabelValues(DependencyOpenBao, op, outcome)
		}
	}
	ConsumedSchemaViolationsTotal.WithLabelValues("tenant_offboarding", "TenantMembershipsPurged")
	for _, result := range []string{"ok", "error"} {
		OffboardingCascadeTotal.WithLabelValues(result)
		RotationSweepTotal.WithLabelValues(result)
	}
	for _, result := range []string{"orphan_deleted", "missing_material", "ok", "error"} {
		MaterialReconcileTotal.WithLabelValues(result)
	}
	ProcessedEventsDuplicates.WithLabelValues("tenant_offboarding")
	DuplicateMessagesTotal.WithLabelValues("tenant-lifecycle-tokensvc-q", "TenantMembershipsPurged")
}

// Label values this service records on the shared Tier 1 metrics.
const (
	DependencyOpenBao = "openbao"
	OutcomeSuccess    = "success"
	OutcomeError      = "error"
)

// registryEntry returns name's Platform Observability Registry entry; a
// missing entry is a build-time contract break, so it panics at startup.
func registryEntry(name string) *obsregistry.Metric {
	m, ok := obsregistry.Default().Lookup(name)
	if !ok {
		panic(fmt.Sprintf("metrics: %s is not in the Platform Observability Registry", name))
	}
	return m
}

// registerShared registers c on gincommon's registerer, or — when a
// collector with the identical descriptor is already registered (platform-
// events' own, via InitLibraryMetrics) — returns that collector, so both
// libraries record into one series set. Any other registration error (a
// conflicting shape) panics at startup, like MustRegister.
func registerShared[C prometheus.Collector](c C) C {
	err := gincommon.MetricsRegisterer().Register(c)
	if err == nil {
		return c
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if existing, ok := are.ExistingCollector.(C); ok {
			return existing
		}
	}
	panic(fmt.Sprintf("metrics: register shared collector: %v", err))
}
