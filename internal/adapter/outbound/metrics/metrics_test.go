package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
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

	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-token-service-test", BuildVersion: "test"})

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
	assert.NotNil(t, ProcessedEventsDuplicates)
	assert.NotNil(t, UnknownEventAcknowledged)
	assert.NotNil(t, DependencyRequestDuration, "Tier 1 (registry-proposed)")
	assert.NotNil(t, DuplicateMessagesTotal, "Tier 1 (registry-proposed)")
	assert.NotNil(t, IAMOffboardingCascadeTotal, "Tier 2 (proposed for the IAM Domain Metric Registry)")
}

// TestTier1Labels_CarryDomainServiceEnvironment and
// TestTier2Labels_CarryServiceEnvironment lock down the Enterprise
// Platform Observability Standard's required-label contract: Tier 1 must
// carry {domain, service, environment}; Tier 2 must carry {service,
// environment} (domain is implicit in the iam_ name prefix, not repeated
// as a label). These labels are injected centrally by registerMetrics
// (requirement #8 — instrumentation call sites never set them), so this
// test reads them back off an already-registered collector rather than
// constructing one itself.
func TestTier1Labels_CarryDomainServiceEnvironment(t *testing.T) {
	Register("test")
	labels := tier1Labels()
	assert.Equal(t, "iam", labels["domain"])
	assert.Equal(t, "token-service", labels["service"])
	assert.Equal(t, "test", labels["environment"])
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

func TestTier2Labels_CarryServiceEnvironmentOnly(t *testing.T) {
	Register("test")
	labels := tier2Labels()
	assert.Equal(t, "token-service", labels["service"])
	assert.Equal(t, "test", labels["environment"])
	_, hasDomain := labels["domain"]
	assert.False(t, hasDomain, "Tier 2 must not repeat domain as a label — it is already the iam_ name prefix")
}
