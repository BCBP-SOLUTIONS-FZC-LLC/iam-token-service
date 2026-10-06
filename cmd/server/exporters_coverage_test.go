package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// warnLogger is a port.Logger that records every Warn call's message and
// fields, so the exporters' failure logging (including the trace_id
// enrichment) can be asserted on.
type warnLogger struct {
	mu     sync.Mutex
	warns  []string
	fields []map[string]interface{}
}

func (*warnLogger) Debug(string, map[string]interface{}) {}
func (*warnLogger) Info(string, map[string]interface{})  {}
func (*warnLogger) Error(string, map[string]interface{}) {}
func (l *warnLogger) Warn(msg string, f map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
	l.fields = append(l.fields, f)
}

func (l *warnLogger) snapshot() ([]string, []map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...), append([]map[string]interface{}(nil), l.fields...)
}

// tracedCtx returns a context carrying a valid (remote) span context. The
// default global tracer provider is a no-op, which still propagates a valid
// parent span context into its child spans, so gincommon.SpanTraceID inside
// the exporter sees this trace id without mutating any global state.
func tracedCtx(t *testing.T) (context.Context, string) {
	t.Helper()
	traceID := trace.TraceID{0x0a, 0x0b, 0x0c, 0x0d, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	require.True(t, sc.IsValid())
	return trace.ContextWithRemoteSpanContext(t.Context(), sc), traceID.String()
}

func TestRLSViolationExporter_WarnLogsWithTraceID(t *testing.T) {
	metrics.Register("test")
	ctx, traceID := tracedCtx(t)
	log := &warnLogger{}
	e := &rlsViolationExporter{repo: &fakeRLSViolations{cursorErr: errors.New("db down")}, window: time.Minute, log: log}

	e.tick(ctx)

	warns, fields := log.snapshot()
	require.Equal(t, []string{"rls violation exporter: query failed"}, warns)
	assert.Equal(t, "db down", fields[0]["error"])
	assert.Equal(t, traceID, fields[0]["trace_id"])
}

func TestRLSViolationExporter_WarnLogsWithoutTraceID(t *testing.T) {
	metrics.Register("test")
	log := &warnLogger{}
	e := &rlsViolationExporter{repo: &fakeRLSViolations{cursorBefore: 3, countErr: errors.New("timeout")}, window: time.Minute, log: log}

	e.tick(t.Context())

	warns, fields := log.snapshot()
	require.Len(t, warns, 1, "a CountSince failure is logged too")
	assert.Equal(t, "timeout", fields[0]["error"])
	assert.NotContains(t, fields[0], "trace_id", "no span context → no trace_id field")
	assert.True(t, e.initialized, "the cursor init succeeded before the count failed")
}

// countingRLSViolations counts CursorBefore/CountSince calls atomically so
// the run loop can be observed from another goroutine.
type countingRLSViolations struct {
	counts atomic.Int32
}

func (*countingRLSViolations) CursorBefore(context.Context, time.Duration) (int64, error) {
	return 0, nil
}

func (c *countingRLSViolations) CountSince(_ context.Context, afterID int64) (map[string]int64, int64, error) {
	c.counts.Add(1)
	return nil, afterID, nil
}

func TestRunRLSViolationExporter_TicksImmediatelyThenPeriodicallyUntilCancelled(t *testing.T) {
	metrics.Register("test")
	repo := &countingRLSViolations{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runRLSViolationExporter(ctx, repo, 5*time.Millisecond, nil)
		close(done)
	}()
	require.Eventually(t, func() bool { return repo.counts.Load() >= 3 }, 5*time.Second, time.Millisecond,
		"an immediate tick plus ticker-driven ones")
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exporter did not stop on context cancellation")
	}
}

func TestRunKeysRefreshPendingExporter_RefreshesOnEveryTick(t *testing.T) {
	metrics.Register("test")
	repo := &fakeKeysRefreshStats{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runKeysRefreshPendingExporter(ctx, repo, 5*time.Millisecond, nil)
		close(done)
	}()
	require.Eventually(t, func() bool { return repo.calls.Load() >= 3 }, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exporter did not stop on context cancellation")
	}
}

func TestRefreshKeysRefreshPendingGauges_WarnLogsWithAndWithoutTraceID(t *testing.T) {
	metrics.Register("test")
	repo := &fakeKeysRefreshStats{err: errors.New("db down")}
	log := &warnLogger{}

	ctx, traceID := tracedCtx(t)
	refreshKeysRefreshPendingGauges(ctx, repo, log)
	refreshKeysRefreshPendingGauges(t.Context(), repo, log)

	warns, fields := log.snapshot()
	require.Equal(t, []string{
		"keys refresh pending exporter: query failed",
		"keys refresh pending exporter: query failed",
	}, warns)
	assert.Equal(t, "db down", fields[0]["error"])
	assert.Equal(t, traceID, fields[0]["trace_id"])
	assert.NotContains(t, fields[1], "trace_id")
}

// newOutboxRunner only stores the pool pointer at construction (the outbox
// store first touches it in Run), so a zero-value *pgcommon.Pool is enough to
// exercise the wiring without a database.
func TestNewOutboxRunner_BuildsWithServiceDefaults(t *testing.T) {
	t.Setenv("OUTBOX_POLL_INTERVAL", "")
	t.Setenv("OUTBOX_PUBLISH_CONCURRENCY", "")
	t.Setenv("OUTBOX_STARTUP_JITTER", "")
	t.Setenv("OUTBOX_CLAIM_LEASE_DURATION", "")
	runner, err := newOutboxRunner(&pgcommon.Pool{}, noopPublisher{}, &warnLogger{})
	require.NoError(t, err)
	assert.NotNil(t, runner)
}

func TestNewOutboxRunner_RejectsTooShortClaimLease(t *testing.T) {
	t.Setenv("OUTBOX_CLAIM_LEASE_DURATION", "1s")
	runner, err := newOutboxRunner(&pgcommon.Pool{}, noopPublisher{}, &warnLogger{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ClaimLeaseDuration")
	assert.Nil(t, runner)
}
