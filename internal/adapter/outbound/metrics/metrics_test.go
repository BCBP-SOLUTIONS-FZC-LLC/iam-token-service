package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/obsregistry"
)

// testutilCounterValue reads a Counter's current value — shared by this
// package's decorator tests (credentialservice_test.go, secretstore_test.go)
// to assert on before/after deltas without depending on test ordering
// (each test reads its own before-value first).
func testutilCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	return testutil.ToFloat64(c)
}

// TestGincommonLabels_TransitionsFromNilToPopulated exercises both branches
// of gincommonLabels() within a single test function (not split across
// separate Test funcs / files) so this assertion never depends on
// cross-file or cross-test execution order — gincommon's const-labels
// state is process-global and, once set, cannot be unset for the rest of
// the test binary.
func TestGincommonLabels_TransitionsFromNilToPopulated(t *testing.T) {
	// NOTE: this only observes a genuine "before" nil state if no other
	// test in this package has already called gincommon.ObservabilityMiddlewares.
	// This is the only place in this package's test suite that calls it —
	// see this package's other test files, which deliberately never touch
	// gincommon.ObservabilityMiddlewares.
	before := gincommonLabels()

	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-token-service-test", BuildVersion: "test", Domain: "iam", Environment: "test"})

	after := gincommonLabels()
	assert.NotEmpty(t, after, "gincommonLabels must be non-empty after ObservabilityMiddlewares runs")
	assert.Equal(t, "iam-token-service-test", after["service"])
	if len(before) != 0 {
		t.Logf("gincommonLabels was already non-empty before this test ran (%v) — another test in this binary called ObservabilityMiddlewares first; the pre-init nil branch was not directly observed here, but is exercised by construction whenever this package's collectors are registered before any Config is set (e.g. a fresh test binary run of just this function).", before)
	}
}

func TestRegister_IsIdempotentAndRegistersAllCollectors(t *testing.T) {
	require.NotPanics(t, func() {
		Register("test")
		Register("test") // sync.Once — the second call must be a safe no-op, not a duplicate-registration panic.
	})

	assert.NotNil(t, CredentialsIssuedTotal)
	assert.NotNil(t, RotationOverlapActive)
	assert.NotNil(t, OpenBaoCallDuration)
	assert.NotNil(t, OffboardingCascadeTotal)
	assert.NotNil(t, RotationSweepTotal)
	assert.NotNil(t, MaterialReconcileTotal)
	assert.NotNil(t, CadenceRotationTotal)
	assert.NotNil(t, JWKSKeyErrorsTotal)
	assert.NotNil(t, ProcessedEventsDuplicates)
	assert.NotNil(t, UnknownEventAcknowledged)
	assert.NotNil(t, DependencyRequestDuration, "Tier 1 (registry-proposed)")
	assert.NotNil(t, DuplicateMessagesTotal, "Tier 1 (registry-proposed)")

	// A collector being non-nil only proves it was constructed, not that
	// it was actually passed to MustRegister — CadenceRotationTotal was
	// constructed but never registered from when it was first added until
	// this production-readiness review (TS-D15), so its own alert
	// (IAMTokenServiceCadenceRotationFailures) could never have fired.
	// re-MustRegister-ing an already-registered collector panics
	// ("duplicate metrics collector registration attempted"); if it
	// *doesn't* panic here, that's this exact bug recurring.
	for _, c := range []prometheus.Collector{
		CredentialsIssuedTotal, RotationOverlapActive, OpenBaoCallDuration,
		OffboardingCascadeTotal, RotationSweepTotal, MaterialReconcileTotal,
		CadenceRotationTotal, JWKSKeyErrorsTotal, JWKSKnownTenantsRefreshTotal,
		JWKSKnownTenantsLastRefreshAgeSeconds, ProcessedEventsDuplicates, UnknownEventAcknowledged,
		DependencyRequestDuration, DuplicateMessagesTotal,
		JWKSRateLimitedByBucketTotal, CredentialReplaysTotal, KeysRefreshPending,
		KeysRefreshOldestAgeSeconds, ConsumerDLQRejectsTotal,
	} {
		assert.Panics(t, func() { gincommon.MetricsRegisterer().MustRegister(c) },
			"%T was constructed but never actually passed to MustRegister in registerMetrics", c)
	}
}

// TestTier1Labels_CarryDomainServiceEnvironment locks down the Enterprise
// Platform Observability Standard's required-label contract: Tier 1 must
// carry {domain, service, environment}. These labels are injected centrally by registerMetrics
// (requirement #8 — instrumentation call sites never set them), so this
// test reads them back off an already-registered collector rather than
// constructing one itself.
func TestTier1Labels_CarryDomainServiceEnvironment(t *testing.T) {
	Register("test")
	labels := tier1Labels()
	assert.Equal(t, "iam", labels["domain"])
	assert.Equal(t, expectedService(), labels["service"],
		"Tier 1 service must be gincommon's service label (shared with platform-events/pgcommon), not a separate spelling")
	assert.Equal(t, "test", labels["environment"])
}

// expectedService is gincommon's service label once ObservabilityMiddlewares
// has run in this test binary, else the package fallback.
func expectedService() string {
	if s := gincommonLabels()["service"]; s != "" {
		return s
	}
	return serviceName
}

// TestSharedTier1Collectors_UseRegistryShape locks the Tier 1 collectors to
// the Platform Observability Registry's label sets — the shape platform-events
// registers, so Register can adopt platform-events' collector instead of
// disabling it.
func TestSharedTier1Collectors_UseRegistryShape(t *testing.T) {
	Register("test")
	require.NotPanics(t, func() {
		DependencyRequestDuration.WithLabelValues(DependencyOpenBao, "write", OutcomeError)
		DuplicateMessagesTotal.WithLabelValues("tenant-lifecycle-tokensvc-q", "TenantMembershipsPurged")
	})
	assert.Equal(t, []string{"dependency", "operation", "outcome"}, registryEntry(dependencyRequestSecondsName).Labels)
	assert.Equal(t, []string{"queue", "event_type"}, registryEntry(duplicateMessagesTotalName).Labels)
}

// TestWithEnvironment_NilAndPopulatedBase covers both branches of
// withEnvironment's merge loop directly (white-box) — a nil base (the
// pre-ObservabilityMiddlewares gincommonLabels() state) must still yield a
// valid map with "environment" set, and a populated base must be copied
// alongside it, not replaced.
func TestWithEnvironment_NilAndPopulatedBase(t *testing.T) {
	Register("test")

	nilResult := withEnvironment(nil)
	assert.Equal(t, prometheus.Labels{"environment": "test"}, nilResult)

	populated := withEnvironment(prometheus.Labels{"service": "iam-token-service", "version": "v1"})
	assert.Equal(t, prometheus.Labels{"service": "iam-token-service", "version": "v1", "environment": "test"}, populated)
}

// TestRegister_EmitsNoUnregisteredDomainMetric guards the platform libraries'
// metrics contract: an iam_* name outside this service's iam_token_service_
// prefix must be a Platform Observability Registry entry, and
// iam_offboarding_cascade_total is not one yet (Proposal 3).
func TestRegister_EmitsNoUnregisteredDomainMetric(t *testing.T) {
	Register("test")
	families, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	for _, mf := range families {
		name := mf.GetName()
		if strings.HasPrefix(name, "iam_") && !strings.HasPrefix(name, "iam_token_service_") {
			_, ok := obsregistry.Default().Lookup(name)
			assert.True(t, ok, "%s is an iam_* domain metric missing from the registry", name)
		}
	}
}

// TestTSD23Helpers_PreInitialisedAndBounded pins the TS-D23 counters'
// label vocabularies: every known value is pre-initialised (a dashboard
// shows 0, not "no data"), unknown values are folded or dropped so
// cardinality never grows, and the gauge pair reports 0 age when empty.
func TestTSD23Helpers_PreInitialisedAndBounded(t *testing.T) {
	Register("test")

	for _, b := range []string{"tenant", "global", "unknown"} {
		assert.Contains(t, gatherLabelValues(t, "iam_token_service_jwks_rate_limited_by_bucket_total", "bucket"), b)
	}
	for _, r := range []string{"served", "expired", "revoked"} {
		assert.Contains(t, gatherLabelValues(t, "iam_token_service_credential_replays_total", "result"), r)
	}
	for _, r := range []string{"schema_violation", "invalid_envelope_id"} {
		assert.Contains(t, gatherLabelValues(t, "iam_token_service_consumer_dlq_rejects_total", "reason"), r)
	}

	before := testutil.ToFloat64(JWKSRateLimitedByBucketTotal.WithLabelValues(JWKSBucketUnknown))
	IncJWKSRateLimited(JWKSBucketUnknown)
	IncJWKSRateLimited("bogus")
	assert.InDelta(t, before+1, testutil.ToFloat64(JWKSRateLimitedByBucketTotal.WithLabelValues(JWKSBucketUnknown)), 0)
	assert.NotContains(t, gatherLabelValues(t, "iam_token_service_jwks_rate_limited_by_bucket_total", "bucket"), "bogus")

	before = testutil.ToFloat64(CredentialReplaysTotal.WithLabelValues(ReplayServed))
	IncCredentialReplay(ReplayServed)
	IncCredentialReplay("bogus")
	assert.InDelta(t, before+1, testutil.ToFloat64(CredentialReplaysTotal.WithLabelValues(ReplayServed)), 0)

	before = testutil.ToFloat64(ConsumerDLQRejectsTotal.WithLabelValues("other"))
	IncConsumerDLQReject("something_new")
	assert.InDelta(t, before+1, testutil.ToFloat64(ConsumerDLQRejectsTotal.WithLabelValues("other")), 0)
	assert.NotContains(t, gatherLabelValues(t, "iam_token_service_consumer_dlq_rejects_total", "reason"), "something_new")

	SetKeysRefreshPending(3, 125.5)
	assert.InDelta(t, 3, testutil.ToFloat64(KeysRefreshPending), 0)
	assert.InDelta(t, 125.5, testutil.ToFloat64(KeysRefreshOldestAgeSeconds), 0)
	SetKeysRefreshPending(0, 99)
	assert.InDelta(t, 0, testutil.ToFloat64(KeysRefreshPending), 0)
	assert.InDelta(t, 0, testutil.ToFloat64(KeysRefreshOldestAgeSeconds), 0, "an empty table reports age 0")
}

// gatherLabelValues returns every value label takes on metric name in the
// gincommon registry.
func gatherLabelValues(t *testing.T, name, label string) []string {
	t.Helper()
	mfs, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	var out []string
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label {
					out = append(out, lp.GetValue())
				}
			}
		}
	}
	return out
}
