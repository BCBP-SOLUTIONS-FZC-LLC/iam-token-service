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
//     outside the domain. An iam_* name must be a Platform Observability
//     Registry entry before it is emitted: this package emits
//     iam_rls_violations_total (shared with iam-org-membership,
//     iam-user-profile and iam-audit-log); the proposed
//     iam_offboarding_cascade_total waits on ratification.
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
	"slices"
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

	// Tier 2 registry name this service emits.
	rlsViolationsTotalName = "iam_rls_violations_total"
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

	// JWKSRateLimitedTotal counts JWKS requests answered 429 by the
	// per-tenant or process-wide limiter. Alerted: a rate-limited Keycloak
	// fetch right after RP-17 is an auth outage for that tenant.
	JWKSRateLimitedTotal prometheus.Counter

	// JWKSRateLimitedByBucketTotal is JWKSRateLimitedTotal split by the
	// bucket that denied the request (tenant|global|unknown, TS-D23): a
	// `tenant` 429 is one caller hammering one URL, a `global` 429 means
	// known tenants together exceed the process-wide budget, and an
	// `unknown` 429 is a random-tenant-id flood (harmless to known tenants).
	// Incremented alongside the unlabelled counter, which stays unchanged.
	// Use IncJWKSRateLimited.
	JWKSRateLimitedByBucketTotal *prometheus.CounterVec

	// CredentialReplaysTotal counts TS-1 rotation_id replays (§9.2) by
	// result: served (the private key was handed out again), expired
	// (refused, past ROTATION_REPLAY_WINDOW) or revoked (refused, the
	// credential is gone). A replay re-reads a live private key, so each
	// one is also an audit log line (TS-D23). Use IncCredentialReplay.
	CredentialReplaysTotal *prometheus.CounterVec

	// KeysRefreshPending is the number of rows in keys_refresh_pending —
	// tenants owed an RP-17 Keycloak key-cache refresh (TS-D22). Nonzero is
	// normal for a few minutes after an RP-17 failure; a value that does not
	// drain means Keycloak still trusts a revoked key or lacks a new one.
	// Use SetKeysRefreshPending.
	KeysRefreshPending prometheus.Gauge

	// KeysRefreshOldestAgeSeconds is the age of the oldest
	// keys_refresh_pending row (0 when the table is empty) — the alertable
	// signal: how long Keycloak has been out of sync for the worst tenant.
	KeysRefreshOldestAgeSeconds prometheus.Gauge

	// ConsumerDLQRejectsTotal counts inbound messages the consumer sent
	// straight to the -dlq as permanent rejects, by reason
	// (schema_violation|invalid_envelope_id|other, TS-D22/TS-D23). Each one
	// is an offboarding cascade that did not run. Use IncConsumerDLQReject.
	ConsumerDLQRejectsTotal *prometheus.CounterVec

	// ProcessedEventsDuplicates counts SQS redeliveries the platform-events
	// inbox found already claimed in processed_events, matching
	// iam-org-membership. Authoritative until the Tier 1
	// platform_duplicate_messages_total (counted by the inbox) is ratified.
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

	// ── Tier 2: iam_* (registry entries, shared across IAM services) ─────

	// RLSViolations is iam_rls_violations_total{violation_type}: RLS checks
	// that failed (cross_tenant_access, missing_or_invalid_guc), sampled into
	// rls_violation_log by rls_check_tenant() (migration 000003) and counted
	// by cmd/server's exporter. Use AddRLSViolations.
	RLSViolations *prometheus.CounterVec

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
	// skipped. Recorded by platform-events' inbox itself (the consumer's
	// dedup runs on pkg/inbox); this handle exists so Register adopts the
	// library's collector and pre-warms its series. The service records only
	// the Tier 3 ProcessedEventsDuplicates.
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
		Help:        "Inbound SQS redeliveries the platform-events inbox found already claimed in processed_events, by consumer. Authoritative until platform_duplicate_messages_total (counted by the inbox) is ratified.",
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

	// ── Tier 2 (registry entry: name, help and labels are the registry's) ─
	RLSViolations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        rlsViolationsTotalName,
		Help:        registryEntry(rlsViolationsTotalName).Help,
		ConstLabels: t1,
	}, registryEntry(rlsViolationsTotalName).Labels)

	JWKSKeyErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        tier3Prefix + "jwks_key_errors_total",
		Help:        "Live credentials the JWKS route (EXT-6) could not serve — OpenBao material unreadable/unparseable. Page-worthy: a real per-credential Keycloak auth outage.",
		ConstLabels: t3,
	})

	JWKSRateLimitedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name:        tier3Prefix + "jwks_rate_limited_total",
		Help:        "JWKS route requests rejected with 429 by the per-tenant or process-wide rate limiter. Sustained increase means a caller is flooding the route and Keycloak's key fetches may be starved.",
		ConstLabels: t3,
	})

	JWKSRateLimitedByBucketTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "jwks_rate_limited_by_bucket_total",
		Help:        "JWKS route 429s by the token bucket that denied them (tenant|global|unknown). unknown alone is a random-tenant-id flood that cannot affect known tenants.",
		ConstLabels: t3,
	}, []string{"bucket"})

	CredentialReplaysTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "credential_replays_total",
		Help:        "TS-1 rotation_id replays by result (served|expired|revoked). served re-handed out a live private key.",
		ConstLabels: t3,
	}, []string{"result"})

	KeysRefreshPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        tier3Prefix + "keys_refresh_pending",
		Help:        "Tenants owed an RP-17 Keycloak key-cache refresh (rows in keys_refresh_pending).",
		ConstLabels: t3,
	})

	KeysRefreshOldestAgeSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        tier3Prefix + "keys_refresh_oldest_age_seconds",
		Help:        "Age of the oldest keys_refresh_pending row in seconds (0 when none is owed).",
		ConstLabels: t3,
	})

	ConsumerDLQRejectsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        tier3Prefix + "consumer_dlq_rejects_total",
		Help:        "Inbound messages sent straight to the -dlq as permanent rejects, by reason (schema_violation|invalid_envelope_id|other).",
		ConstLabels: t3,
	}, []string{"reason"})

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
		JWKSRateLimitedTotal,
		JWKSRateLimitedByBucketTotal,
		CredentialReplaysTotal,
		KeysRefreshPending,
		KeysRefreshOldestAgeSeconds,
		ConsumerDLQRejectsTotal,
		ProcessedEventsDuplicates,
		UnknownEventAcknowledged,
		ConsumedSchemaViolationsTotal,
		RLSViolations,
	)

	// Pre-initialize labels so dashboards show 0 rather than "no data".
	for _, op := range []string{"issue", "rotate", "revoke"} {
		CredentialsIssuedTotal.WithLabelValues(op)
	}
	for _, op := range []string{"write", "delete"} {
		OpenBaoCallDuration.WithLabelValues(op)
	}
	for _, op := range []string{"write", "delete", "read", "list"} {
		for _, outcome := range []string{OutcomeSuccess, OutcomeError} {
			DependencyRequestDuration.WithLabelValues(DependencyOpenBao, op, outcome)
		}
	}
	for _, outcome := range []string{OutcomeSuccess, OutcomeError} {
		DependencyRequestDuration.WithLabelValues(DependencyRealmProvisioner, OperationRefreshKeys, outcome)
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
	for _, vType := range rlsViolationTypes {
		RLSViolations.WithLabelValues(vType)
	}
	DuplicateMessagesTotal.WithLabelValues("tenant-lifecycle-tokensvc-q", "TenantMembershipsPurged")
	for _, b := range jwksBuckets {
		JWKSRateLimitedByBucketTotal.WithLabelValues(b)
	}
	for _, r := range replayResults {
		CredentialReplaysTotal.WithLabelValues(r)
	}
	for _, r := range dlqRejectReasons {
		ConsumerDLQRejectsTotal.WithLabelValues(r)
	}
}

// Label vocabularies of the TS-D23 counters. Helpers fold anything else to
// "other" (DLQ reasons) or drop it (bucket/replay result — a programming
// error, never a caller-controlled value), so cardinality stays fixed.
const (
	JWKSBucketTenant  = "tenant"
	JWKSBucketGlobal  = "global"
	JWKSBucketUnknown = "unknown"

	ReplayServed  = "served"
	ReplayExpired = "expired"
	ReplayRevoked = "revoked"

	DLQReasonSchemaViolation   = "schema_violation"
	DLQReasonInvalidEnvelopeID = "invalid_envelope_id"
	dlqReasonOther             = "other"
)

var (
	jwksBuckets      = []string{JWKSBucketTenant, JWKSBucketGlobal, JWKSBucketUnknown}
	replayResults    = []string{ReplayServed, ReplayExpired, ReplayRevoked}
	dlqRejectReasons = []string{DLQReasonSchemaViolation, DLQReasonInvalidEnvelopeID}
)

// IncJWKSRateLimited increments jwks_rate_limited_by_bucket_total{bucket}.
// It does NOT touch the unlabelled JWKSRateLimitedTotal; the handler keeps
// incrementing that one itself. Nil-safe; an unknown bucket is ignored.
func IncJWKSRateLimited(bucket string) {
	if JWKSRateLimitedByBucketTotal == nil || !slices.Contains(jwksBuckets, bucket) {
		return
	}
	JWKSRateLimitedByBucketTotal.WithLabelValues(bucket).Inc()
}

// IncCredentialReplay increments credential_replays_total{result}.
// Nil-safe; an unknown result is ignored.
func IncCredentialReplay(result string) {
	if CredentialReplaysTotal == nil || !slices.Contains(replayResults, result) {
		return
	}
	CredentialReplaysTotal.WithLabelValues(result).Inc()
}

// SetKeysRefreshPending sets keys_refresh_pending to count and
// keys_refresh_oldest_age_seconds to oldestAgeSeconds (clamped at 0, and
// forced to 0 when count is 0 so an empty table never reports a stale
// age). Nil-safe.
func SetKeysRefreshPending(count int, oldestAgeSeconds float64) {
	if KeysRefreshPending == nil || KeysRefreshOldestAgeSeconds == nil {
		return
	}
	if count <= 0 || oldestAgeSeconds < 0 {
		oldestAgeSeconds = 0
	}
	if count < 0 {
		count = 0
	}
	KeysRefreshPending.Set(float64(count))
	KeysRefreshOldestAgeSeconds.Set(oldestAgeSeconds)
}

// IncConsumerDLQReject increments consumer_dlq_rejects_total{reason},
// folding any reason outside the known set to "other". Nil-safe.
func IncConsumerDLQReject(reason string) {
	if ConsumerDLQRejectsTotal == nil {
		return
	}
	if !slices.Contains(dlqRejectReasons, reason) {
		reason = dlqReasonOther
	}
	ConsumerDLQRejectsTotal.WithLabelValues(reason).Inc()
}

// Label values this service records on the shared Tier 1 metrics.
const (
	DependencyOpenBao          = "openbao"
	DependencyRealmProvisioner = "realm_provisioner"
	OperationRefreshKeys       = "refresh_keys"
	OutcomeSuccess             = "success"
	OutcomeError               = "error"
)

// rlsViolationTypes is the violation_type set rls_check_tenant() writes
// (migration 000003's CHECK constraint), plus the registry's catch-all.
var rlsViolationTypes = []string{"cross_tenant_access", "missing_or_invalid_guc", "other"}

// AddRLSViolations adds n to iam_rls_violations_total{violation_type},
// mapping any value outside the registry vocabulary to "other". Nil-safe.
func AddRLSViolations(violationType string, n float64) {
	if RLSViolations == nil || n <= 0 {
		return
	}
	if !slices.Contains(rlsViolationTypes, violationType) {
		violationType = "other"
	}
	RLSViolations.WithLabelValues(violationType).Add(n)
}

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
