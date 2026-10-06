package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

type fakeRLSViolations struct {
	cursorBefore int64
	cursorErr    error
	rows         map[int64]string // id → violation_type
	countErr     error
	afterIDs     []int64
}

func (f *fakeRLSViolations) CursorBefore(context.Context, time.Duration) (int64, error) {
	return f.cursorBefore, f.cursorErr
}

func (f *fakeRLSViolations) CountSince(_ context.Context, afterID int64) (map[string]int64, int64, error) {
	f.afterIDs = append(f.afterIDs, afterID)
	if f.countErr != nil {
		return nil, afterID, f.countErr
	}
	counts, last := map[string]int64{}, afterID
	for id, vType := range f.rows {
		if id > afterID {
			counts[vType]++
			last = max(last, id)
		}
	}
	return counts, last, nil
}

func rlsCount(vType string) float64 {
	return testutil.ToFloat64(metrics.RLSViolations.WithLabelValues(vType))
}

func TestRLSViolationExporter_CountsEachRowOnce(t *testing.T) {
	metrics.Register("test")
	repo := &fakeRLSViolations{cursorBefore: 2, rows: map[int64]string{
		1: "cross_tenant_access", // before the startup window: not counted
		2: "cross_tenant_access",
		3: "cross_tenant_access",
		4: "missing_or_invalid_guc",
		5: "unexpected_type",
	}}
	e := &rlsViolationExporter{repo: repo, window: time.Minute}
	crossBefore, gucBefore, otherBefore := rlsCount("cross_tenant_access"), rlsCount("missing_or_invalid_guc"), rlsCount("other")

	e.tick(t.Context())
	e.tick(t.Context()) // nothing new: must not re-add

	assert.InDelta(t, 1, rlsCount("cross_tenant_access")-crossBefore, 0)
	assert.InDelta(t, 1, rlsCount("missing_or_invalid_guc")-gucBefore, 0)
	assert.InDelta(t, 1, rlsCount("other")-otherBefore, 0, "unknown types map to the registry's other")
	assert.Equal(t, []int64{2, 5}, repo.afterIDs, "the cursor starts at CursorBefore and advances to the last id")
}

func TestRLSViolationExporter_RetriesCursorInitAfterError(t *testing.T) {
	metrics.Register("test")
	repo := &fakeRLSViolations{cursorErr: errors.New("db down")}
	e := &rlsViolationExporter{repo: repo, window: time.Minute}

	e.tick(t.Context())
	require.False(t, e.initialized)
	assert.Empty(t, repo.afterIDs, "no count before the cursor is known")

	repo.cursorErr = nil
	e.tick(t.Context())
	assert.True(t, e.initialized)
	assert.Equal(t, []int64{0}, repo.afterIDs)
}

func TestRLSViolationExporter_KeepsCursorOnCountError(t *testing.T) {
	metrics.Register("test")
	repo := &fakeRLSViolations{cursorBefore: 7, countErr: errors.New("timeout")}
	e := &rlsViolationExporter{repo: repo, window: time.Minute}

	e.tick(t.Context())
	assert.Equal(t, int64(7), e.cursor)
}

type fakeKeysRefreshStats struct {
	count int
	age   time.Duration
	err   error
	calls atomic.Int32
}

func (f *fakeKeysRefreshStats) PendingStats(context.Context) (int, time.Duration, error) {
	f.calls.Add(1)
	return f.count, f.age, f.err
}

func TestKeysRefreshPendingExporter_SetsGaugesAndKeepsLastOnError(t *testing.T) {
	metrics.Register("test")
	pending := func() (float64, float64) {
		return gaugeValue(t, "iam_token_service_keys_refresh_pending"), gaugeValue(t, "iam_token_service_keys_refresh_oldest_age_seconds")
	}
	repo := &fakeKeysRefreshStats{count: 3, age: 90 * time.Second}

	refreshKeysRefreshPendingGauges(t.Context(), repo, nil)
	n, age := pending()
	assert.InDelta(t, 3, n, 0)
	assert.InDelta(t, 90, age, 0)

	repo.count, repo.age, repo.err = 0, 0, errors.New("db down")
	refreshKeysRefreshPendingGauges(t.Context(), repo, nil)
	n, age = pending()
	assert.InDelta(t, 3, n, 0, "a failed query keeps the last value rather than reading as 'nothing pending'")
	assert.InDelta(t, 90, age, 0)

	repo.err = nil
	refreshKeysRefreshPendingGauges(t.Context(), repo, nil)
	n, age = pending()
	assert.InDelta(t, 0, n, 0)
	assert.InDelta(t, 0, age, 0)
}

func TestRunKeysRefreshPendingExporter_TicksUntilCancelled(t *testing.T) {
	metrics.Register("test")
	repo := &fakeKeysRefreshStats{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runKeysRefreshPendingExporter(ctx, repo, time.Hour, nil)
		close(done)
	}()
	require.Eventually(t, func() bool { return repo.calls.Load() == 1 }, time.Second, time.Millisecond, "refreshes immediately at start")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("exporter did not stop on context cancellation")
	}
}

// gaugeValue reads an unlabelled gauge by name from gincommon's registry,
// the registerer metrics.Register writes to.
func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := gincommon.MetricsGatherer().Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name {
			require.Len(t, mf.GetMetric(), 1)
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s not registered", name)
	return 0
}
