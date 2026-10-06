package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// When the registry already holds a platform_* name with a different shape,
// platform-events / platform-pgcommon disable that one metric and return a
// RegistrationWarning instead of failing; InitLibraryMetrics logs each one
// (naming the metric) and still succeeds.
//
// This file sorts after metrics_test.go, so ObservabilityMiddlewares below
// (same identity as TestGincommonLabels_TransitionsFromNilToPopulated, so a
// no-op if that already ran) never pre-empts that test's pre-init check.
func TestInitLibraryMetrics_LogsRegistrationWarnings(t *testing.T) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-token-service-test", BuildVersion: "test", Domain: "iam", Environment: "test"})
	reg := gincommon.MetricsRegisterer()

	// Same names as one platform-events and one platform-pgcommon metric,
	// but a different type and label set.
	const evName, pgName = "platform_outbox_errors_total", "platform_db_slow_queries_total"
	squatters := []prometheus.Collector{
		prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: evName, Help: "squatter"}, []string{"x"}),
		prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: pgName, Help: "squatter"}, []string{"x"}),
	}
	for _, c := range squatters {
		require.NoError(t, reg.Register(c))
	}
	t.Cleanup(func() {
		for _, c := range squatters {
			reg.Unregister(c)
		}
	})

	log := &warnRecorder{}
	require.NoError(t, InitLibraryMetrics(log))

	var events, pg []string
	for i, msg := range log.msgs {
		switch msg {
		case "platform-events metric disabled":
			events = append(events, log.fields[i]["metric"].(string))
		case "platform-pgcommon metric disabled":
			pg = append(pg, log.fields[i]["metric"].(string))
		}
		assert.NotEmpty(t, log.fields[i]["error"], "each warning carries the registration error")
	}
	assert.Contains(t, events, evName)
	assert.Contains(t, pg, pgName)
}
