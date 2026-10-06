package metrics

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// warnRecorder is a port.Logger that records Warn calls.
type warnRecorder struct {
	msgs   []string
	fields []map[string]interface{}
}

func (*warnRecorder) Debug(string, map[string]interface{}) {}
func (*warnRecorder) Info(string, map[string]interface{})  {}
func (w *warnRecorder) Warn(msg string, fields map[string]interface{}) {
	w.msgs = append(w.msgs, msg)
	w.fields = append(w.fields, fields)
}
func (*warnRecorder) Error(string, map[string]interface{}) {}

// InitLibraryMetrics before gincommon.ObservabilityMiddlewares is a startup
// ordering bug and must fail loudly, not register platform_* metrics with an
// empty identity.
//
// gincommon's identity is process-global and can't be unset, so this only
// observes the pre-init state when it runs before any test in this binary
// that calls ObservabilityMiddlewares — which it does in the default
// (source-file) order: this file sorts before metrics_test.go and
// tier1_warnings_coverage_test.go, the only callers.
func TestInitLibraryMetrics_BeforeObservabilityMiddlewaresErrors(t *testing.T) {
	if len(gincommon.MetricsConstLabels()) != 0 {
		t.Skip("gincommon's identity is already set in this test binary; the pre-init state can't be observed")
	}
	err := InitLibraryMetrics(&warnRecorder{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ObservabilityMiddlewares must run first")
}

func TestWarn_NilLoggerIsNoop(t *testing.T) {
	assert.NotPanics(t, func() { warn(nil, "msg", "platform_x", errors.New("boom")) })
}

func TestWarn_LogsMetricAndOptionalError(t *testing.T) {
	log := &warnRecorder{}
	warn(log, "with error", "platform_a", errors.New("boom"))
	warn(log, "without error", "platform_b", nil)

	require.Len(t, log.fields, 2)
	assert.Equal(t, []string{"with error", "without error"}, log.msgs)
	assert.Equal(t, map[string]interface{}{"metric": "platform_a", "error": "boom"}, log.fields[0])
	assert.Equal(t, map[string]interface{}{"metric": "platform_b"}, log.fields[1], "no error key when err is nil")
}

// swapNil sets *p to its zero value for the test and restores it afterwards.
func swapNil[T any](t *testing.T, p *T) {
	t.Helper()
	saved := *p
	var zero T
	*p = zero
	t.Cleanup(func() { *p = saved })
}

// Every helper is nil-safe: before Register (cmd/rotator, cmd/scheduler and
// unit tests that never register) a call is a silent no-op, never a panic.
func TestHelpers_NilCollectorsAreNoops(t *testing.T) {
	Register("test")
	swapNil(t, &KeysRefreshPending)
	swapNil(t, &ConsumerDLQRejectsTotal)
	swapNil(t, &RLSViolations)
	swapNil(t, &DependencyRequestDuration)

	assert.NotPanics(t, func() {
		SetKeysRefreshPending(3, 10)
		IncConsumerDLQReject("schema_violation")
		AddRLSViolations("cross_tenant_access", 1)
		observeDependency(DependencyOpenBao, "write_secret", nil, 0.1)
	})
}

// A negative count (a defensive clamp — COUNT(*) never is) reports 0 rows
// and 0 age.
func TestSetKeysRefreshPending_NegativeCountClampsToZero(t *testing.T) {
	Register("test")
	SetKeysRefreshPending(-2, 50)
	assert.InDelta(t, 0, testutil.ToFloat64(KeysRefreshPending), 0)
	assert.InDelta(t, 0, testutil.ToFloat64(KeysRefreshOldestAgeSeconds), 0)
}

// AddRLSViolations ignores a non-positive delta and folds an unknown
// violation_type into "other" so the label set stays bounded.
func TestAddRLSViolations_BoundsDeltaAndVocabulary(t *testing.T) {
	Register("test")
	other := RLSViolations.WithLabelValues("other")
	cross := RLSViolations.WithLabelValues("cross_tenant_access")
	beforeOther, beforeCross := testutil.ToFloat64(other), testutil.ToFloat64(cross)

	AddRLSViolations("cross_tenant_access", 0)
	AddRLSViolations("cross_tenant_access", -1)
	assert.InDelta(t, beforeCross, testutil.ToFloat64(cross), 0, "non-positive deltas are ignored")

	AddRLSViolations("cross_tenant_access", 2)
	AddRLSViolations("brand_new_type", 3)
	assert.InDelta(t, beforeCross+2, testutil.ToFloat64(cross), 0)
	assert.InDelta(t, beforeOther+3, testutil.ToFloat64(other), 0)
	assert.NotContains(t, gatherLabelValues(t, "iam_rls_violations_total", "violation_type"), "brand_new_type")
}

// A metric missing from the Platform Observability Registry is a build-time
// contract break: registryEntry panics so the binary never starts.
func TestRegistryEntry_UnknownMetricPanics(t *testing.T) {
	assert.PanicsWithValue(t,
		"metrics: iam_token_service_not_a_real_metric_total is not in the Platform Observability Registry",
		func() { registryEntry("iam_token_service_not_a_real_metric_total") })
}

// registerShared adopts an identical existing collector, but a same-name
// collector with a different shape is a startup panic (like MustRegister).
func TestRegisterShared_ConflictingShapePanics(t *testing.T) {
	const name = "iam_token_service_registershared_conflict_test_total"
	first := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: "first"}, []string{"a"})
	got := registerShared(first)
	require.Same(t, first, got)
	t.Cleanup(func() { gincommon.MetricsRegisterer().Unregister(first) })

	identical := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: "first"}, []string{"a"})
	assert.Same(t, first, registerShared(identical), "an identical descriptor adopts the existing collector")

	conflicting := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: "first"}, []string{"b"})
	assert.Panics(t, func() { registerShared(conflicting) })
}
