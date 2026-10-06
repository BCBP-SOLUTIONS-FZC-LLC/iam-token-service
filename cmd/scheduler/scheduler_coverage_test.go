package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// TestMain registers the business metrics once for the package's test
// binary — recordCadenceScanMetric would otherwise panic against the nil
// collectors metrics.Register populates (mirrors cmd/rotator's TestMain).
func TestMain(m *testing.M) {
	metrics.Register("test")
	m.Run()
}

// covLogger records every Warn/Error call (message + fields).
type covLogger struct {
	mu     sync.Mutex
	warns  []string
	errs   []string
	fields []map[string]any
}

func (l *covLogger) Debug(string, map[string]any) {}
func (l *covLogger) Info(string, map[string]any)  {}
func (l *covLogger) Warn(msg string, f map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
	l.fields = append(l.fields, f)
}

func (l *covLogger) Error(msg string, f map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, msg)
	l.fields = append(l.fields, f)
}

var _ port.Logger = (*covLogger)(nil)

// stubMarkers is a keysRefreshMarkers whose every call can be failed — the
// in-memory fakeKeysRefreshMarkers never fails Clear/ListPending and always
// reports HasPrincipal.
type stubMarkers struct {
	pending  []pgadapter.PendingKeysRefresh
	listErr  error
	clearErr error
	cleared  []uuid.UUID
	fresh    bool
}

func (s *stubMarkers) MarkIntent(context.Context, uuid.UUID, time.Duration) (pgadapter.KeysRefreshMark, error) {
	return pgadapter.KeysRefreshMark{RequestedAt: time.Unix(1_700_000_000, 0), Fresh: s.fresh}, nil
}

func (s *stubMarkers) MarkPending(context.Context, uuid.UUID) (pgadapter.KeysRefreshMark, error) {
	return pgadapter.KeysRefreshMark{RequestedAt: time.Unix(1_700_000_001, 0)}, nil
}

func (s *stubMarkers) Clear(_ context.Context, tenantID uuid.UUID, _ time.Time) error {
	s.cleared = append(s.cleared, tenantID)
	return s.clearErr
}

func (s *stubMarkers) ListPending(context.Context, int) ([]pgadapter.PendingKeysRefresh, error) {
	return s.pending, s.listErr
}

var _ keysRefreshMarkers = (*stubMarkers)(nil)

func tracedCtx(t *testing.T) (context.Context, string) {
	t.Helper()
	tid, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	require.NoError(t, err)
	sid, err := trace.SpanIDFromHex("0102030405060708")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), sc), tid.String()
}

// ── helpers.go ─────────────────────────────────────────────────────────

func TestStartJobSpan_DefaultsServiceNameAndEnds(t *testing.T) {
	ctx, end := startJobSpan(context.Background(), "", "cadence_scan")
	require.NotNil(t, ctx)
	assert.NotPanics(t, end)

	ctx2, end2 := startJobSpan(context.Background(), "iam-token-service-test", "cadence_scan")
	require.NotNil(t, ctx2)
	assert.NotPanics(t, end2)
}

func TestRecordCadenceScanMetric(t *testing.T) {
	rotated := testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("rotated"))
	skipped := testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("skipped"))
	failed := testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("failed"))

	recordCadenceScanMetric(cadenceScanResult{Rotated: 3, Skipped: 2, Failed: 1})
	assert.Equal(t, rotated+3, testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("rotated")))
	assert.Equal(t, skipped+2, testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("skipped")))
	assert.Equal(t, failed+1, testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("failed")))

	// All-zero (Deferred only) records nothing.
	recordCadenceScanMetric(cadenceScanResult{Deferred: 4})
	assert.Equal(t, rotated+3, testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("rotated")))
	assert.Equal(t, skipped+2, testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("skipped")))
	assert.Equal(t, failed+1, testutil.ToFloat64(metrics.CadenceRotationTotal.WithLabelValues("failed")))
}

func TestFieldsWithTrace(t *testing.T) {
	assert.Nil(t, fieldsWithTrace(context.Background(), nil))
	assert.Equal(t, map[string]any{"k": "v"}, fieldsWithTrace(context.Background(), map[string]any{"k": "v"}))

	ctx, traceID := tracedCtx(t)
	assert.Equal(t, map[string]any{"trace_id": traceID}, fieldsWithTrace(ctx, nil))
	assert.Equal(t, map[string]any{"k": "v", "trace_id": traceID}, fieldsWithTrace(ctx, map[string]any{"k": "v"}))
}

func TestLogError(t *testing.T) {
	assert.NotPanics(t, func() { logError(context.Background(), nil, "dropped", nil, errors.New("boom")) })

	log := &covLogger{}
	ctx, traceID := tracedCtx(t)
	logError(ctx, log, "op failed", nil, errors.New("boom"))
	require.Equal(t, []string{"op failed"}, log.errs)
	assert.Equal(t, map[string]any{"error": "boom", "trace_id": traceID}, log.fields[0])

	logError(context.Background(), log, "op failed again", map[string]any{"tenant_id": "t1"}, errors.New("bang"))
	assert.Equal(t, map[string]any{"tenant_id": "t1", "error": "bang"}, log.fields[1])
}

// ── main.go env helpers / serveMetricsBriefly ──────────────────────────

func TestEnvOr(t *testing.T) {
	t.Setenv("TS_SCHED_ENVOR", "value")
	assert.Equal(t, "value", envOr("TS_SCHED_ENVOR", "def"))
	t.Setenv("TS_SCHED_ENVOR", "")
	assert.Equal(t, "def", envOr("TS_SCHED_ENVOR", "def"))
}

func TestEnvInt(t *testing.T) {
	for raw, want := range map[string]int{"42": 42, "": 7, "x": 7, "0": 7, "-3": 7} {
		t.Setenv("TS_SCHED_ENVINT", raw)
		assert.Equal(t, want, envInt("TS_SCHED_ENVINT", 7), "raw=%q", raw)
	}
}

func TestEnvDuration(t *testing.T) {
	for raw, want := range map[string]time.Duration{"5s": 5 * time.Second, "": time.Minute, "x": time.Minute, "0s": time.Minute, "-5s": time.Minute} {
		t.Setenv("TS_SCHED_ENVDUR", raw)
		assert.Equal(t, want, envDuration("TS_SCHED_ENVDUR", time.Minute), "raw=%q", raw)
	}
}

func TestIsDevEnv(t *testing.T) {
	for _, env := range []string{"dev", "development", "local", "test"} {
		assert.True(t, isDevEnv(env), env)
	}
	for _, env := range []string{"production", "staging", ""} {
		assert.False(t, isDevEnv(env), env)
	}
}

func TestMustEnv(t *testing.T) {
	t.Setenv("TS_SCHED_MUSTENV", "value")
	assert.Equal(t, "value", mustEnv("TS_SCHED_MUSTENV", "production", "dev-default"))
	t.Setenv("TS_SCHED_MUSTENV", "")
	assert.Equal(t, "dev-default", mustEnv("TS_SCHED_MUSTENV", "dev", "dev-default"))
	assert.PanicsWithValue(t, "startup aborted — required env var TS_SCHED_MUSTENV is not set",
		func() { mustEnv("TS_SCHED_MUSTENV", "production", "dev-default") })
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	require.NoError(t, l.Close())
	return port
}

func TestServeMetricsBriefly_GraceElapsesThenShutsDown(t *testing.T) {
	port := freePort(t)
	t.Setenv("METRICS_PORT", port)
	log := &covLogger{}

	done := make(chan struct{})
	go func() {
		serveMetricsBriefly(log, 50*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveMetricsBriefly did not return after its grace window")
	}
	assert.Empty(t, log.warns, "a clean shutdown logs nothing")

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected the metrics server to be shut down")
	}
}

func TestServeMetricsBriefly_ListenErrorIsLogged(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	t.Setenv("METRICS_PORT", port)

	log := &covLogger{}
	serveMetricsBriefly(log, 5*time.Second)
	assert.Equal(t, []string{"scheduler metrics server error"}, log.warns)
}

// A client that declares a request body and never sends it keeps its
// connection active, so Shutdown's 5s budget expires and the shutdown error
// is logged (mirrors cmd/rotator's test). Takes ~5s by construction.
func TestServeMetricsBriefly_ShutdownTimeoutIsLogged(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out serveMetricsBriefly's 5s shutdown budget")
	}
	port := freePort(t)
	t.Setenv("METRICS_PORT", port)
	log := &covLogger{}

	done := make(chan struct{})
	go func() {
		serveMetricsBriefly(log, 500*time.Millisecond)
		close(done)
	}()

	var conn net.Conn
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 50*time.Millisecond)
		if err != nil {
			return false
		}
		conn = c
		return true
	}, 2*time.Second, 10*time.Millisecond)
	defer func() { _ = conn.Close() }()
	_, err := conn.Write([]byte("POST /metrics HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serveMetricsBriefly did not return after its shutdown budget")
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	require.Equal(t, []string{"scheduler metrics server shutdown error"}, log.warns)
	assert.Equal(t, context.DeadlineExceeded.Error(), log.fields[0]["error"])
}

// ── keys_refresh.go ────────────────────────────────────────────────────

func TestRetryPendingKeyRefreshes_ListErrorFails(t *testing.T) {
	log := &covLogger{}
	res := retryPendingKeyRefreshes(context.Background(), &stubMarkers{listErr: errors.New("db down")}, newFakeRealmProvisionerClient(), 10, log)
	assert.Equal(t, pendingRefreshResult{Failed: 1}, res)
	assert.Equal(t, []string{"keys refresh retry: list pending markers failed"}, log.errs)
}

func TestRetryPendingKeyRefreshes_OutcomesPerMarker(t *testing.T) {
	ok, stillFailing, offboarded := uuid.New(), uuid.New(), uuid.New()
	markers := &stubMarkers{pending: []pgadapter.PendingKeysRefresh{
		{TenantID: ok, HasPrincipal: true},
		{TenantID: stillFailing, HasPrincipal: true},
		{TenantID: offboarded, HasPrincipal: false},
	}}
	rp := newFakeRealmProvisionerClient()
	rp.errs[stillFailing] = errors.New("rp down")
	rp.errs[offboarded] = errors.New("client gone")
	log := &covLogger{}

	res := retryPendingKeyRefreshes(context.Background(), markers, rp, 10, log)

	assert.Equal(t, pendingRefreshResult{Refreshed: 1, Failed: 1, Dropped: 1}, res)
	assert.Equal(t, []uuid.UUID{ok, offboarded}, markers.cleared, "the success clears; the offboarded marker is dropped; the still-failing one is kept")
	assert.Contains(t, log.errs, "keys refresh retry: RP-17 key-cache refresh still failing — marker kept for the next run")
	assert.Contains(t, log.warns, "keys refresh retry: RP-17 failed for a tenant with no principal left — marker dropped")
}

func TestRetryPendingKeyRefreshes_DroppingOffboardedMarkerFails(t *testing.T) {
	offboarded := uuid.New()
	markers := &stubMarkers{pending: []pgadapter.PendingKeysRefresh{{TenantID: offboarded}}, clearErr: errors.New("db down")}
	rp := newFakeRealmProvisionerClient()
	rp.errs[offboarded] = errors.New("client gone")
	log := &covLogger{}

	res := retryPendingKeyRefreshes(context.Background(), markers, rp, 10, log)

	assert.Equal(t, pendingRefreshResult{Failed: 1}, res)
	assert.Equal(t, []string{"keys refresh retry: dropping the marker of an offboarded tenant failed"}, log.errs)
}

func TestRetryPendingKeyRefreshes_BudgetSpentDefersRest(t *testing.T) {
	markers := &stubMarkers{pending: []pgadapter.PendingKeysRefresh{
		{TenantID: uuid.New(), HasPrincipal: true},
		{TenantID: uuid.New(), HasPrincipal: true},
	}}
	rp := newFakeRealmProvisionerClient()
	log := &covLogger{}
	// The pass gets half the remaining budget: half of 10s is below
	// rp17Timeout, so not even the first marker may start.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res := retryPendingKeyRefreshes(ctx, markers, rp, 10, log)

	assert.Equal(t, pendingRefreshResult{Deferred: 2}, res)
	assert.Empty(t, rp.order, "no RP-17 call once the budget is spent")
	assert.Equal(t, []string{"keys refresh retry: budget spent — leaving the remaining markers to the next run"}, log.warns)
	assert.Equal(t, 2, log.fields[0]["deferred"])
}

func TestRetryPendingKeyRefreshes_CancelledDefersWithNilLogger(t *testing.T) {
	markers := &stubMarkers{pending: []pgadapter.PendingKeysRefresh{{TenantID: uuid.New(), HasPrincipal: true}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := retryPendingKeyRefreshes(ctx, markers, newFakeRealmProvisionerClient(), 10, nil)
	assert.Equal(t, pendingRefreshResult{Deferred: 1}, res)
}

func TestRetryPendingKeyRefreshes_AmpleDeadlineRefreshes(t *testing.T) {
	tenant := uuid.New()
	markers := &stubMarkers{pending: []pgadapter.PendingKeysRefresh{{TenantID: tenant, HasPrincipal: true}}}
	rp := newFakeRealmProvisionerClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	res := retryPendingKeyRefreshes(ctx, markers, rp, 10, nil)
	assert.Equal(t, pendingRefreshResult{Refreshed: 1}, res)
	assert.Equal(t, 1, rp.refreshed[tenant])
}

func TestRefreshAndClear_ClearFailureIsLoggedNotReturned(t *testing.T) {
	tenant := uuid.New()
	log := &covLogger{}
	rp := newFakeRealmProvisionerClient()
	err := refreshAndClear(context.Background(), &stubMarkers{clearErr: errors.New("db down")}, rp, tenant, time.Now(), log)
	require.NoError(t, err)
	assert.Equal(t, 1, rp.refreshed[tenant])
	assert.Equal(t, []string{"keys refresh: RP-17 succeeded but clearing its marker failed — the next run repeats the (harmless) refresh"}, log.warns)
}

func TestWorkCtx_KeepsDeadlineDropsCancellation(t *testing.T) {
	deadline := time.Now().Add(time.Hour)
	parent, cancel := context.WithDeadline(context.Background(), deadline)
	wctx, wcancel := workCtx(parent)
	defer wcancel()
	cancel()

	got, ok := wctx.Deadline()
	require.True(t, ok)
	assert.Equal(t, deadline, got)
	assert.NoError(t, wctx.Err(), "the parent's cancellation must not reach the in-flight unit")
}

// ── scan.go ────────────────────────────────────────────────────────────

func TestRunCadenceScan_DeferralIsLogged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{
		{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1},
		{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1},
	}}
	log := &covLogger{}
	issuer := &fakeCredentialIssuer{}

	res := runCadenceScan(ctx, rr, issuer, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 10, log)

	assert.Equal(t, cadenceScanResult{Deferred: 2}, res)
	assert.Empty(t, issuer.calls)
	require.Len(t, log.warns, 1)
	assert.Equal(t, 2, log.fields[0]["deferred_rows"])
	assert.Equal(t, 2, log.fields[0]["deferred_tenants"])
}

func TestRunCadenceScan_SignalMidTenantDefersRemainingRows(t *testing.T) {
	tenant := uuid.New()
	p1, p2, p3 := uuid.New(), uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{
		{TenantID: tenant, PrincipalID: p1, Version: 1},
		{TenantID: tenant, PrincipalID: p2, Version: 1},
		{TenantID: tenant, PrincipalID: p3, Version: 1},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	issuer := &fakeCredentialIssuer{onIssue: func(uuid.UUID) { cancel() }}
	rp := newFakeRealmProvisionerClient()

	res := runCadenceScan(ctx, rr, issuer, newFakeKeysRefreshMarkers(), rp, domain.DefaultOverlapSeconds, 10, nil)

	assert.Equal(t, cadenceScanResult{Rotated: 1, Deferred: 2}, res)
	assert.Equal(t, []uuid.UUID{p1}, issuer.calls)
	assert.Equal(t, 1, rp.refreshed[tenant], "the committed rotation still gets its RP-17")
}

func TestRunCadenceScan_WithdrawUnusedMarkerFailureIsLogged(t *testing.T) {
	tenant, principal := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenant, PrincipalID: principal, Version: 1}}}
	issuer := &fakeCredentialIssuer{errs: map[uuid.UUID]error{principal: newRevokedError()}}
	markers := &stubMarkers{fresh: true, clearErr: errors.New("db down")}
	log := &covLogger{}

	res := runCadenceScan(context.Background(), rr, issuer, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 10, log)

	assert.Equal(t, cadenceScanResult{Skipped: 1}, res)
	assert.Equal(t, []uuid.UUID{tenant}, markers.cleared, "the fresh, unused intent marker is withdrawn")
	assert.Equal(t, []string{"cadence scan: withdrawing an unused keys-refresh marker failed — the next run repeats a harmless RP-17"}, log.warns)
}

func TestRemarkCommitted_FailureIsLoggedAndReturnsZero(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	markers.commitMarkErr = errors.New("db down")
	log := &covLogger{}
	row := port.DueForRotation{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1}

	got := remarkCommitted(context.Background(), context.Background(), markers, row, log)

	assert.True(t, got.IsZero())
	assert.Equal(t, []string{"cadence scan: re-marking the keys refresh as committed failed — the intent marker is retried once it expires"}, log.warns)
	assert.Equal(t, row.TenantID, log.fields[0]["tenant_id"])
}
