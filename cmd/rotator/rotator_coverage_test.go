package main

// Branch-coverage tests for cmd/rotator's job functions: the error,
// deferral and logging branches the scenario tests in sweep_test.go,
// orphan_reconciler_test.go, prune_test.go and main_helpers_test.go do not
// reach.

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// levelLogger records every call at every level, so a test can assert
// which level a branch logged at.
type levelLogger struct {
	entries []logEntry
}

type logEntry struct {
	level  string
	msg    string
	fields map[string]interface{}
}

func (l *levelLogger) add(level, msg string, f map[string]interface{}) {
	l.entries = append(l.entries, logEntry{level: level, msg: msg, fields: f})
}
func (l *levelLogger) Debug(msg string, f map[string]interface{}) { l.add("debug", msg, f) }
func (l *levelLogger) Info(msg string, f map[string]interface{})  { l.add("info", msg, f) }
func (l *levelLogger) Warn(msg string, f map[string]interface{})  { l.add("warn", msg, f) }
func (l *levelLogger) Error(msg string, f map[string]interface{}) { l.add("error", msg, f) }

func (l *levelLogger) levels() []string {
	out := make([]string, 0, len(l.entries))
	for _, e := range l.entries {
		out = append(out, e.level)
	}
	return out
}

var _ port.Logger = (*levelLogger)(nil)

// clearFailMarkers fails every Clear; everything else is the in-memory fake.
type clearFailMarkers struct {
	*fakeKeysRefreshMarkers
	clearErr error
}

func (c *clearFailMarkers) Clear(context.Context, uuid.UUID, time.Time) error { return c.clearErr }

// findFailCredentials fails every FindByVersion.
type findFailCredentials struct {
	*fakeCredentialRepository
	findErr error
}

func (f *findFailCredentials) FindByVersion(context.Context, uuid.UUID, uuid.UUID, int) (*domain.Credential, error) {
	return nil, f.findErr
}

// liveFailReconciler fails every IsCredentialLive.
type liveFailReconciler struct {
	*fakeReconcilerRepository
	liveErr error
}

func (r *liveFailReconciler) IsCredentialLive(context.Context, uuid.UUID, int) (bool, error) {
	return false, r.liveErr
}

// cancellingInboxPruner cancels the run (SIGTERM mid-call) and fails with
// the resulting context error.
type cancellingInboxPruner struct{ cancel context.CancelFunc }

func (c *cancellingInboxPruner) Prune(ctx context.Context, _ port.ProcessedEventsConsumer, _ time.Duration, _ int) (int64, error) {
	c.cancel()
	return 0, ctx.Err()
}

// fullThenCancelRLSPruner returns one full batch, then cancels the run and
// fails with the context error on the next call.
type fullThenCancelRLSPruner struct {
	cancel context.CancelFunc
	calls  int
}

func (p *fullThenCancelRLSPruner) Prune(ctx context.Context, _, limit int) (int, error) {
	p.calls++
	if p.calls == 1 {
		return limit, nil
	}
	p.cancel()
	return 0, ctx.Err()
}

// ---- helpers.go ----

func TestFieldsWithTrace_ValidSpanAddsTraceID(t *testing.T) {
	traceID := trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}})
	require.True(t, sc.IsValid())
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	got := fieldsWithTrace(ctx, nil)
	assert.Equal(t, map[string]interface{}{"trace_id": traceID.String()}, got, "a nil map is allocated to carry trace_id")

	got = fieldsWithTrace(ctx, map[string]interface{}{"k": "v"})
	assert.Equal(t, map[string]interface{}{"k": "v", "trace_id": traceID.String()}, got)
}

// ---- keys_refresh.go ----

func TestRetryPendingKeyRefreshes_DeadlineWithRoomRefreshesAll(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	a, b := uuid.New(), uuid.New()
	_, _ = markers.MarkPending(context.Background(), a)
	_, _ = markers.MarkPending(context.Background(), b)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	rp := newFakeRealmProvisionerClient()

	result := retryPendingKeyRefreshes(ctx, markers, rp, 500, nil)

	assert.Equal(t, pendingRefreshResult{Refreshed: 2}, result)
	assert.ElementsMatch(t, []uuid.UUID{a, b}, rp.refreshed)
	assert.False(t, markers.isPending(a))
	assert.False(t, markers.isPending(b))
}

// The retry pass gets half the remaining budget: with 15s left the phase
// ends in 7.5s, less than one rp17Timeout, so nothing is attempted.
func TestRetryPendingKeyRefreshes_HalfBudgetTooSmallDefersAndWarns(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	tenantID := uuid.New()
	_, _ = markers.MarkPending(context.Background(), tenantID)
	ctx, cancel := context.WithTimeout(context.Background(), rp17Timeout+5*time.Second)
	defer cancel()
	rp := newFakeRealmProvisionerClient()
	log := &levelLogger{}

	result := retryPendingKeyRefreshes(ctx, markers, rp, 500, log)

	assert.Equal(t, pendingRefreshResult{Deferred: 1}, result)
	assert.Empty(t, rp.refreshed)
	assert.True(t, markers.isPending(tenantID))
	require.Equal(t, []string{"warn"}, log.levels())
	assert.Equal(t, 1, log.entries[0].fields["deferred"])
}

func TestRetryPendingKeyRefreshes_DroppedMarkerIsWarned(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	tenantID := uuid.New()
	_, _ = markers.MarkPending(context.Background(), tenantID)
	markers.hasPrin[tenantID] = false
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("404 client not found")
	log := &levelLogger{}

	result := retryPendingKeyRefreshes(context.Background(), markers, rp, 500, log)

	assert.Equal(t, pendingRefreshResult{Dropped: 1}, result)
	require.Equal(t, []string{"warn"}, log.levels())
	assert.Equal(t, tenantID, log.entries[0].fields["tenant_id"])
	assert.Equal(t, "404 client not found", log.entries[0].fields["error"])
}

func TestRetryPendingKeyRefreshes_DroppingMarkerFailsIsFailed(t *testing.T) {
	base := newFakeKeysRefreshMarkers()
	tenantID := uuid.New()
	_, _ = base.MarkPending(context.Background(), tenantID)
	base.hasPrin[tenantID] = false
	markers := &clearFailMarkers{fakeKeysRefreshMarkers: base, clearErr: errors.New("db down")}
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("404 client not found")
	log := &levelLogger{}

	result := retryPendingKeyRefreshes(context.Background(), markers, rp, 500, log)

	assert.Equal(t, pendingRefreshResult{Failed: 1}, result)
	assert.True(t, base.isPending(tenantID), "the marker survives a failed drop")
	require.Equal(t, []string{"error"}, log.levels())
	assert.Equal(t, "db down", log.entries[0].fields["error"])
}

// RP-17 landed but clearing the marker failed: success is still returned
// (the leftover marker only costs a harmless repeat), with a warning.
func TestRefreshAndClear_ClearFailureIsWarnedNotReturned(t *testing.T) {
	base := newFakeKeysRefreshMarkers()
	tenantID := uuid.New()
	mark, _ := base.MarkPending(context.Background(), tenantID)
	markers := &clearFailMarkers{fakeKeysRefreshMarkers: base, clearErr: errors.New("db down")}
	rp := newFakeRealmProvisionerClient()
	log := &levelLogger{}

	err := refreshAndClear(context.Background(), markers, rp, tenantID, mark.RequestedAt, log)

	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{tenantID}, rp.refreshed)
	require.Equal(t, []string{"warn"}, log.levels())
	assert.Equal(t, "db down", log.entries[0].fields["error"])
}

func TestWorkCtx_KeepsDeadlineButNotCancellation(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), time.Hour)
	want, _ := parent.Deadline()

	wctx, wcancel := workCtx(parent)
	defer wcancel()
	cancel()

	got, ok := wctx.Deadline()
	require.True(t, ok)
	assert.Equal(t, want, got)
	assert.NoError(t, wctx.Err(), "a signal cancelling the run must not abort the in-flight unit")

	wcancel()
	assert.ErrorIs(t, wctx.Err(), context.Canceled)
}

// ---- orphan_reconciler.go ----

func TestOrphanReconciler_SignalledRunDefersAndWarns(t *testing.T) {
	tenantID := uuid.New()
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(tenantID, uuid.New(), 1, 1), materialState(tenantID, uuid.New(), 1, 1),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(ctx, rr, newFakeSecretStore(), testLock(nil), 0, log)

	assert.Equal(t, materialReconcileResult{Deferred: 2}, result)
	require.Equal(t, []string{"warn"}, log.levels())
	assert.Equal(t, 2, log.entries[0].fields["deferred"])
}

func TestOrphanReconciler_OpenBaoListFailureFailsThePrincipal(t *testing.T) {
	secrets := newFakeSecretStore()
	secrets.listErr = errors.New("openbao unavailable")
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, log)

	assert.Equal(t, materialReconcileResult{Failed: 1}, result, "no missing_material verdict from a failed listing")
	require.Equal(t, []string{"error"}, log.levels())
	assert.Equal(t, principalID, log.entries[0].fields["principal_id"])
}

func TestOrphanReconciler_IgnoresNonVersionKeys(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "meta", "v0", "vx/"}
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, materialReconcileResult{OK: 1}, result)
	assert.Empty(t, secrets.deleted)
}

// A version recorded under an older prefix (a re-mint changed the client
// id) that also appears under the current prefix is left for the older
// prefix's own check — never treated as an orphan of the current one.
func TestOrphanReconciler_SameVersionUnderAnotherPrefixIsNotAnOrphan(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	oldPath := domain.OpenBaoPathFor(tenantID, "old-client", 1)
	oldPrefix := "iam/serviceaccount/" + tenantID.String() + "/old-client"
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1"}
	secrets.byPrefix[oldPrefix] = []string{"v1"}
	st := port.PrincipalMaterialState{
		TenantID: tenantID, PrincipalID: principalID, KeycloakClientID: domain.KeycloakClientPlatformAutomation, MaxVersion: 1,
		Credentials: []port.CredentialMaterial{{Version: 1, OpenBaoPath: oldPath, Live: true}},
	}
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{st}}
	lock := testLock(nil)

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, nil)

	assert.Equal(t, materialReconcileResult{OK: 1}, result)
	assert.Empty(t, secrets.deleted)
	assert.Empty(t, lock.principals.(*fakePrincipalLocker).lockedBy, "no orphan candidate, so no lock taken")
}

func TestOrphanReconciler_LivenessRecheckFailureIsFailed(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &liveFailReconciler{
		fakeReconcilerRepository: &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}},
		liveErr:                  errors.New("reconciler pool down"),
	}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, log)

	assert.Equal(t, materialReconcileResult{Failed: 1}, result)
	require.Equal(t, []string{"error"}, log.levels())
	assert.Equal(t, 1, log.entries[0].fields["version"])
}

func TestOrphanReconciler_MissingCheckReadFailureIsFailed(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}
	credentials := &findFailCredentials{fakeCredentialRepository: newFakeCredentialRepository(), findErr: errors.New("db down")}
	lock := orphanLock{tx: &fakeTxRunner{}, principals: &fakePrincipalLocker{}, credentials: credentials}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, log)

	assert.Equal(t, materialReconcileResult{Failed: 1}, result)
	require.Equal(t, []string{"error"}, log.levels())
	assert.Equal(t, "db down", log.entries[0].fields["error"])
}

// A row with no stored openbao_path falls back to the snapshot's path; the
// missing-material verdict is logged at error level (page-worthy).
func TestOrphanReconciler_MissingMaterialFallsBackToSnapshotPathAndLogs(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}
	credentials := newFakeCredentialRepository()
	cred := liveCredential(tenantID, principalID, 1)
	cred.OpenBaoPath = ""
	credentials.seed(cred)
	lockedLists := &recordingSecretStore{fakeSecretStore: secrets}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, lockedLists, testLock(credentials), 0, log)

	assert.Equal(t, materialReconcileResult{MissingMaterial: 1}, result)
	assert.Equal(t, []string{prefixFor(tenantID), prefixFor(tenantID)}, lockedLists.listed,
		"the locked re-check lists the snapshot path's directory")
	require.Equal(t, []string{"error"}, log.levels())
	assert.Equal(t, principalID, log.entries[0].fields["principal_id"])
}

func TestOrphanReconciler_MissingCheckBusyLockIsSkippedAndLogged(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}
	lock := testLock(nil)
	lock.principals.(*fakePrincipalLocker).errs = map[uuid.UUID]error{
		principalID: domain.NewError(domain.ErrPrincipalNotFound, "gone"),
	}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, log)

	assert.Equal(t, materialReconcileResult{Skipped: 1}, result)
	require.Equal(t, []string{"info"}, log.levels())
	assert.Equal(t, string(domain.ErrPrincipalNotFound), log.entries[0].fields["reason"])
}

func TestOrphanReconciler_OrphanReadFailureUnderLockIsFailed(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1"}
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 0)}}
	credentials := &findFailCredentials{fakeCredentialRepository: newFakeCredentialRepository(), findErr: errors.New("db down")}
	lock := orphanLock{tx: &fakeTxRunner{}, principals: &fakePrincipalLocker{}, credentials: credentials}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, log)

	assert.Equal(t, materialReconcileResult{Failed: 1}, result)
	assert.Empty(t, secrets.deleted, "no delete without a successful re-check")
	require.Equal(t, []string{"error"}, log.levels())
}

func TestOrphanReconciler_OrphanDeleteFailureIsFailed(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1"}
	orphan := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)
	secrets.delErrPaths[orphan] = errors.New("openbao 503")
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 0)}}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, log)

	assert.Equal(t, materialReconcileResult{Failed: 1}, result)
	require.Equal(t, []string{"error"}, log.levels())
	assert.Equal(t, "openbao 503", log.entries[0].fields["error"])
	assert.Equal(t, 1, log.entries[0].fields["version"])
}

func TestOrphanReconciler_OrphanBusyLockIsSkippedAndLogged(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1"}
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 0)}}
	lock := testLock(nil)
	lock.principals.(*fakePrincipalLocker).errs = map[uuid.UUID]error{
		principalID: domain.NewError(domain.ErrRotationInFlight, "lock timeout"),
	}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, log)

	assert.Equal(t, materialReconcileResult{Skipped: 1}, result)
	assert.Empty(t, secrets.deleted)
	require.Equal(t, []string{"info"}, log.levels())
	assert.Equal(t, string(domain.ErrRotationInFlight), log.entries[0].fields["reason"])
}

func TestOrphanReconciler_RevokedMaterialDeleteFailureIsFailed(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1"}
	stale := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)
	secrets.delErrPaths[stale] = errors.New("openbao 503")
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		withRevoked(materialState(tenantID, principalID, 1), 1),
	}}
	log := &levelLogger{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, log)

	assert.Equal(t, materialReconcileResult{Failed: 1}, result)
	assert.Empty(t, secrets.deleted)
	require.Equal(t, []string{"error"}, log.levels())
}

// ---- prune.go ----

func TestRunPrune_ProcessedEventsCutOffBySignalIsDeferred(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outbox := &fakeOutboxPruner{}

	result := runPrune(ctx, &cancellingInboxPruner{cancel: cancel}, outbox, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, pruneResult{Deferred: 2}, result, "processed_events deferred mid-call, outbox never started")
}

func TestRunRLSViolationPrune_SignalMidLoopKeepsCountAndIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &fullThenCancelRLSPruner{cancel: cancel}

	deleted, failed := runRLSViolationPrune(ctx, p, 30, 10, nil)

	assert.False(t, failed)
	assert.Equal(t, 10, deleted, "the committed first batch is still reported")
	assert.Equal(t, 2, p.calls)
}

// ---- sweep.go ----

func TestRunOverlapSweep_SignalledRunDefersAndWarns(t *testing.T) {
	tenantID := uuid.New()
	rr := &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{
		expiredRow(tenantID, uuid.New(), 1, "p1"), expiredRow(tenantID, uuid.New(), 2, "p2"),
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	log := &levelLogger{}

	result := runOverlapSweep(ctx, rr, newFakeCredentialRepository(), newFakeSecretStore(), &fakeTxRunner{},
		newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), 100, log)

	assert.Equal(t, sweepResult{Deferred: 2}, result)
	require.Equal(t, []string{"warn"}, log.levels())
	assert.Equal(t, 2, log.entries[0].fields["deferred_rows"])
	assert.Equal(t, 1, log.entries[0].fields["deferred_tenants"])
}

func TestRevokeExpiredRotating_ErrorBranches(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name  string
		setup func(c *fakeCredentialRepository, m *fakeKeysRefreshMarkers) port.CredentialRepository
	}{
		{"read fails", func(c *fakeCredentialRepository, _ *fakeKeysRefreshMarkers) port.CredentialRepository {
			return &findFailCredentials{fakeCredentialRepository: c, findErr: boom}
		}},
		{"update fails (not an optimistic-lock race)", func(c *fakeCredentialRepository, _ *fakeKeysRefreshMarkers) port.CredentialRepository {
			c.forceUpdateErr = boom
			return c
		}},
		{"marker write fails", func(c *fakeCredentialRepository, m *fakeKeysRefreshMarkers) port.CredentialRepository {
			m.markErr = boom
			return c
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			credentials := newFakeCredentialRepository()
			secrets := newFakeSecretStore()
			markers := newFakeKeysRefreshMarkers()
			tenantID := uuid.New()
			row := seedRotating(credentials, secrets, tenantID, 1)
			repo := tc.setup(credentials, markers)
			tx := &fakeTxRunner{}

			mark, err := revokeExpiredRotating(context.Background(), repo, secrets, tx, markers, row)

			require.ErrorIs(t, err, boom)
			assert.True(t, mark.IsZero())
			assert.Equal(t, 0, tx.commits)
			assert.False(t, markers.isPending(tenantID))
		})
	}
}

// A non-race revoke failure inside the sweep is Failed (logged), and no
// RP-17 is made for a tenant with nothing revoked.
func TestRunOverlapSweep_RevokeErrorIsFailedWithoutRP17(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tenantID := uuid.New()
	row := seedRotating(credentials, secrets, tenantID, 1)
	credentials.forceUpdateErr = errors.New("db down")
	rp := newFakeRealmProvisionerClient()
	log := &levelLogger{}

	result := runOverlapSweep(context.Background(), &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{row}},
		credentials, secrets, &fakeTxRunner{}, newFakeKeysRefreshMarkers(), rp, 100, log)

	assert.Equal(t, sweepResult{Failed: 1}, result)
	assert.Empty(t, rp.refreshed)
	require.Equal(t, []string{"error"}, log.levels())
	assert.Equal(t, "db down", log.entries[0].fields["error"])
}

// ---- main.go ----

// A client that sends a request with a declared body it never delivers
// keeps its connection active (the server blocks discarding the unread
// body, with no read deadline past the headers), so Shutdown's 5s budget
// expires and the shutdown error is logged. Takes ~5s by construction.
func TestServeMetricsBriefly_ShutdownTimeoutIsLogged(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out serveMetricsBriefly's 5s shutdown budget")
	}
	port := freeTCPPort(t)
	t.Setenv("METRICS_PORT", port)
	log := &levelLogger{}

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
	require.Equal(t, []string{"error"}, log.levels())
	assert.Equal(t, "rotator metrics server shutdown error", log.entries[0].msg)
	assert.Equal(t, context.DeadlineExceeded.Error(), log.entries[0].fields["error"])
}
