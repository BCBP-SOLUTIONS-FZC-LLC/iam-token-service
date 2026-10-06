package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

func expiredRow(tenantID, principalID uuid.UUID, version int, path string) port.ExpiredRotatingCredential {
	return port.ExpiredRotatingCredential{TenantID: tenantID, PrincipalID: principalID, Version: version, OpenBaoPath: path}
}

func TestRunOverlapSweep_RevokesExpiredRotating(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	events := &fakeEventPublisher{}
	tx := &fakeTxRunner{events: events}

	tenantID := uuid.New()
	principalID := uuid.New()
	path := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)
	secrets.data[path] = "material"
	credentials.seed(&domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusRotating, OpenBaoPath: path, RecordVersion: 1,
	})

	rr := &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{expiredRow(tenantID, principalID, 1, path)}}

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), 500, nil)

	assert.Equal(t, 1, result.Revoked)
	assert.Equal(t, 0, result.Skipped)
	assert.Equal(t, 0, result.Failed)

	cred, err := credentials.FindByVersion(context.Background(), tenantID, principalID, 1)
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, domain.CredentialStatusRevoked, cred.Status)

	assert.Contains(t, secrets.deleted, path)
	require.Len(t, events.events, 1)
	assert.Equal(t, domain.EventServiceAccountCredentialRevoked, events.events[0].Type)
}

func TestRunOverlapSweep_AlreadyRevokedIsSkippedNotFailed(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{}

	tenantID := uuid.New()
	principalID := uuid.New()
	path := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)
	credentials.seed(&domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusRevoked, OpenBaoPath: path, RecordVersion: 2,
	})

	rr := &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{expiredRow(tenantID, principalID, 1, path)}}

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), 500, nil)

	assert.Equal(t, 0, result.Revoked)
	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, 0, result.Failed)
	assert.Empty(t, secrets.deleted, "an already-revoked row must not trigger a second OpenBao delete")
}

func TestRunOverlapSweep_OneFailureDoesNotBlockTheRest(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{}

	tenantID1, principalID1 := uuid.New(), uuid.New()
	path1 := domain.OpenBaoPathFor(tenantID1, domain.KeycloakClientPlatformAutomation, 1)
	secrets.delErrPaths[path1] = errors.New("openbao unavailable")
	credentials.seed(&domain.Credential{
		TenantID: tenantID1, PrincipalID: principalID1, Version: 1,
		Status: domain.CredentialStatusRotating, OpenBaoPath: path1, RecordVersion: 1,
	})

	tenantID2, principalID2 := uuid.New(), uuid.New()
	path2 := domain.OpenBaoPathFor(tenantID2, domain.KeycloakClientPlatformAutomation, 1)
	credentials.seed(&domain.Credential{
		TenantID: tenantID2, PrincipalID: principalID2, Version: 1,
		Status: domain.CredentialStatusRotating, OpenBaoPath: path2, RecordVersion: 1,
	})

	rr := &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{
		expiredRow(tenantID1, principalID1, 1, path1),
		expiredRow(tenantID2, principalID2, 1, path2),
	}}

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), 500, nil)

	assert.Equal(t, 1, result.Failed)
	assert.Equal(t, 1, result.Revoked, "a failure on one row must not block the next")
}

// TestRunOverlapSweep_ConcurrentRevokeDuringUpdateIsSkippedNotFailed covers
// revokeExpiredRotating's re-classification of a write-time optimistic-lock
// conflict: unlike TestRunOverlapSweep_AlreadyRevokedIsSkippedNotFailed
// (which catches a race that resolved BEFORE this row was read), this
// simulates a concurrent actor (another rotator run, or a racing TS-2 call)
// winning the race AFTER the read but before this row's own Update commits
// — that must still count as Skipped, not Failed, so
// rotation_sweep_total{result="failed"} doesn't page on-call for a race
// that already converged correctly.
func TestRunOverlapSweep_ConcurrentRevokeDuringUpdateIsSkippedNotFailed(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{}

	tenantID := uuid.New()
	principalID := uuid.New()
	path := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)
	secrets.data[path] = "material"
	credentials.seed(&domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusRotating, OpenBaoPath: path, RecordVersion: 1,
	})
	credentials.forceUpdateErr = domain.NewError(domain.ErrOptimisticLockConflict, "record_version conflict")

	rr := &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{expiredRow(tenantID, principalID, 1, path)}}

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), 500, nil)

	assert.Equal(t, 0, result.Revoked)
	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, 0, result.Failed, "a benign concurrent-revoke race must not count as a sweep failure")
}

// TestRunOverlapSweep_RefreshesKeysAfterRevoke covers the fix for the
// production-readiness audit's blocker #1: a revoke that only updates
// Postgres/OpenBao never actually cuts a key off at Keycloak under EXT-6
// (rev 1.3) — Keycloak keeps validating a cached key indefinitely with no
// TTL. The sweep must call RP-17 (RefreshKeys) for the row's tenant after
// every successful revoke.
func TestRunOverlapSweep_RefreshesKeysAfterRevoke(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{events: &fakeEventPublisher{}}

	tenantID := uuid.New()
	principalID := uuid.New()
	path := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)
	secrets.data[path] = "material"
	credentials.seed(&domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusRotating, OpenBaoPath: path, RecordVersion: 1,
	})

	rr := &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{expiredRow(tenantID, principalID, 1, path)}}
	rp := newFakeRealmProvisionerClient()

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeKeysRefreshMarkers(), rp, 500, nil)

	assert.Equal(t, 1, result.Revoked)
	assert.Equal(t, 0, result.Failed)
	assert.Equal(t, []uuid.UUID{tenantID}, rp.refreshed, "RP-17 must be called for the revoked row's tenant")
}

// TestRunOverlapSweep_RP17FailureIsFailedNotRevoked covers the other half
// of the same fix: the DB revoke already committed by the time RP-17 is
// called (material-first ordering, §9.3), so a failed key-refresh cannot
// be rolled back — it must surface as Failed (page-worthy), not be
// swallowed as a quiet success, since the row will never be enumerated by
// ListExpiredRotating again (revokeExpiredRotating's doc comment).
func TestRunOverlapSweep_RP17FailureIsFailedNotRevoked(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{events: &fakeEventPublisher{}}

	tenantID := uuid.New()
	principalID := uuid.New()
	path := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)
	secrets.data[path] = "material"
	credentials.seed(&domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusRotating, OpenBaoPath: path, RecordVersion: 1,
	})

	rr := &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{expiredRow(tenantID, principalID, 1, path)}}
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("realm provisioner unreachable")

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeKeysRefreshMarkers(), rp, 500, nil)

	assert.Equal(t, 0, result.Revoked)
	assert.Equal(t, 1, result.Failed)

	cred, err := credentials.FindByVersion(context.Background(), tenantID, principalID, 1)
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, domain.CredentialStatusRevoked, cred.Status, "the DB revoke is not rolled back on an RP-17 failure")
}

func TestRunOverlapSweep_ListFailureFailsTheRun(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{}
	rr := &fakeReconcilerRepository{err: errors.New("db unavailable")}

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), 500, nil)

	assert.Equal(t, sweepResult{Failed: 1}, result, "nothing is revoked, and the run must fail rather than look clean")
	assert.Empty(t, secrets.deleted)
}

func TestRunOverlapSweep_DefersRowsWhenBudgetIsSpent(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{}
	rr := &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{
		{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1},
		{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), rowReserve/2)
	defer cancel()

	result := runOverlapSweep(ctx, rr, credentials, secrets, tx, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), 500, nil)

	assert.Equal(t, sweepResult{Deferred: 2}, result, "no row is started with less than rowReserve left")
	assert.Empty(t, secrets.deleted)
}

func TestRunOverlapSweep_RefreshesKeysOncePerTenant(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{}
	tenantID := uuid.New()
	var rows []port.ExpiredRotatingCredential
	for v := 1; v <= 2; v++ {
		principalID := uuid.New()
		c := &domain.Credential{ID: uuid.New(), TenantID: tenantID, PrincipalID: principalID, Version: v,
			Status: domain.CredentialStatusRotating, OpenBaoPath: domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, v)}
		credentials.byID[c.ID] = c
		rows = append(rows, port.ExpiredRotatingCredential{TenantID: tenantID, PrincipalID: principalID, Version: v, OpenBaoPath: c.OpenBaoPath})
	}
	rp := newFakeRealmProvisionerClient()

	result := runOverlapSweep(context.Background(), &fakeReconcilerRepository{expired: rows}, credentials, secrets, tx, newFakeKeysRefreshMarkers(), rp, 500, nil)

	assert.Equal(t, 2, result.Revoked)
	assert.Equal(t, []uuid.UUID{tenantID}, rp.refreshed, "RP-17 clears the whole realm cache — once per tenant per run")
}

// seedRotating seeds one expired `rotating` row and its material and
// returns the enumerated row for it.
func seedRotating(credentials *fakeCredentialRepository, secrets *fakeSecretStore, tenantID uuid.UUID, version int) port.ExpiredRotatingCredential {
	principalID := uuid.New()
	path := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, version)
	secrets.data[path] = "material"
	credentials.seed(&domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: version,
		Status: domain.CredentialStatusRotating, OpenBaoPath: path, RecordVersion: 1,
	})
	return expiredRow(tenantID, principalID, version, path)
}

// Finding: RP-17 must run on a context the run's cancellation (SIGTERM, or
// the deadline) cannot reach once the revoke has committed.
func TestRunOverlapSweep_RP17RunsOnUncancelledContextAfterCommit(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tenantID := uuid.New()
	row := seedRotating(credentials, secrets, tenantID, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx := &fakeTxRunner{afterCommit: cancel} // the run ctx dies right after the commit
	rp := newFakeRealmProvisionerClient()

	result := runOverlapSweep(ctx, &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{row}},
		credentials, secrets, tx, newFakeKeysRefreshMarkers(), rp, 500, nil)

	require.Equal(t, []uuid.UUID{tenantID}, rp.refreshed, "a committed revoke's RP-17 still runs")
	assert.NoError(t, rp.ctxErrs[0], "RP-17 must not inherit the run context's cancellation")
	assert.Equal(t, 1, result.Revoked)
	assert.Equal(t, 0, result.Failed)
}

// Rows are processed tenant by tenant (in first-appearance order) and each
// tenant's RP-17 runs inline, right after its own rows — not in one
// post-loop flush outside the budget.
func TestRunOverlapSweep_GroupsByTenantAndRefreshesInline(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tenantA, tenantB := uuid.New(), uuid.New()
	rows := []port.ExpiredRotatingCredential{
		seedRotating(credentials, secrets, tenantA, 1),
		seedRotating(credentials, secrets, tenantB, 1),
		seedRotating(credentials, secrets, tenantA, 2),
	}
	tx := &fakeTxRunner{}
	rp := newFakeRealmProvisionerClient()
	commitsAtRefresh := map[uuid.UUID]int{}
	rp.onRefresh = func(id uuid.UUID) { commitsAtRefresh[id] = tx.commits }

	result := runOverlapSweep(context.Background(), &fakeReconcilerRepository{expired: rows}, credentials, secrets, tx, newFakeKeysRefreshMarkers(), rp, 500, nil)

	assert.Equal(t, 3, result.Revoked)
	assert.Equal(t, []uuid.UUID{tenantA, tenantB}, rp.refreshed)
	assert.Equal(t, 2, commitsAtRefresh[tenantA], "tenant A's RP-17 follows both of A's revokes and precedes B's")
	assert.Equal(t, 3, commitsAtRefresh[tenantB])
}

// A tenant only starts with room for a row AND its RP-17 call.
func TestRunOverlapSweep_TenantNeedsRoomForItsRP17(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	row := seedRotating(credentials, secrets, uuid.New(), 1)
	ctx, cancel := context.WithTimeout(context.Background(), rowReserve+rp17Timeout/2)
	defer cancel()
	rp := newFakeRealmProvisionerClient()

	result := runOverlapSweep(ctx, &fakeReconcilerRepository{expired: []port.ExpiredRotatingCredential{row}},
		credentials, secrets, &fakeTxRunner{}, newFakeKeysRefreshMarkers(), rp, 500, nil)

	assert.Equal(t, sweepResult{Deferred: 1}, result)
	assert.Empty(t, secrets.deleted)
	assert.Empty(t, rp.refreshed)
}

// SIGTERM (run ctx cancelled) during tenant A: A finishes, including its
// RP-17; tenant B is not started and is Deferred, not Failed.
func TestRunOverlapSweep_SignalStopsNewTenants(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tenantA, tenantB := uuid.New(), uuid.New()
	rows := []port.ExpiredRotatingCredential{
		seedRotating(credentials, secrets, tenantA, 1),
		seedRotating(credentials, secrets, tenantB, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rp := newFakeRealmProvisionerClient()
	rp.onRefresh = func(uuid.UUID) { cancel() } // SIGTERM arrives while A's RP-17 is in flight

	result := runOverlapSweep(ctx, &fakeReconcilerRepository{expired: rows}, credentials, secrets, &fakeTxRunner{}, newFakeKeysRefreshMarkers(), rp, 500, nil)

	assert.Equal(t, sweepResult{Revoked: 1, Deferred: 1}, result)
	assert.Equal(t, []uuid.UUID{tenantA}, rp.refreshed)
	assert.Equal(t, []string{rows[0].OpenBaoPath}, secrets.deleted, "tenant B is untouched")
}

// SIGTERM after a tenant's first commit: its remaining rows are Deferred,
// but the RP-17 for what already committed still runs.
func TestRunOverlapSweep_SignalMidTenantStillRefreshesCommittedRows(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tenantID := uuid.New()
	rows := []port.ExpiredRotatingCredential{
		seedRotating(credentials, secrets, tenantID, 1),
		seedRotating(credentials, secrets, tenantID, 2),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rp := newFakeRealmProvisionerClient()

	result := runOverlapSweep(ctx, &fakeReconcilerRepository{expired: rows}, credentials, secrets, &fakeTxRunner{afterCommit: cancel}, newFakeKeysRefreshMarkers(), rp, 500, nil)

	assert.Equal(t, sweepResult{Revoked: 1, Deferred: 1}, result)
	assert.Equal(t, []uuid.UUID{tenantID}, rp.refreshed)
	assert.NoError(t, rp.ctxErrs[0])
}

// Marker lifecycle: written with the revoke, cleared after RP-17
// succeeds, kept after RP-17 fails, and retried (then cleared) by the
// next run's retry pass.
func TestRunOverlapSweep_MarkerLifecycle(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	markers := newFakeKeysRefreshMarkers()
	okTenant, failTenant := uuid.New(), uuid.New()
	rows := []port.ExpiredRotatingCredential{
		seedRotating(credentials, secrets, okTenant, 1),
		seedRotating(credentials, secrets, failTenant, 1),
	}
	rp := newFakeRealmProvisionerClient()
	rp.errs[failTenant] = errors.New("realm provisioner unreachable")

	result := runOverlapSweep(context.Background(), &fakeReconcilerRepository{expired: rows}, credentials, secrets, &fakeTxRunner{}, markers, rp, 500, nil)

	assert.Equal(t, 1, result.Revoked)
	assert.Equal(t, 1, result.Failed)
	assert.Equal(t, 2, markers.marks, "each revoke writes its tenant's marker")
	assert.False(t, markers.isPending(okTenant), "cleared after a successful RP-17")
	assert.True(t, markers.isPending(failTenant), "kept after a failed RP-17")

	// Next run: RP-17 has recovered.
	delete(rp.errs, failTenant)
	retry := retryPendingKeyRefreshes(context.Background(), markers, rp, 500, nil)

	assert.Equal(t, pendingRefreshResult{Refreshed: 1}, retry)
	assert.False(t, markers.isPending(failTenant))
}

// A marker newer than the value observed before the RP-17 call (a
// concurrent run's request) survives the clear.
func TestRefreshAndClear_KeepsNewerRequest(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	tenantID := uuid.New()
	first, _ := markers.MarkPending(context.Background(), tenantID)
	rp := newFakeRealmProvisionerClient()
	rp.onRefresh = func(id uuid.UUID) { _, _ = markers.MarkPending(context.Background(), id) } // written mid-call

	require.NoError(t, refreshAndClear(context.Background(), markers, rp, tenantID, first.RequestedAt, nil))

	assert.True(t, markers.isPending(tenantID), "a request newer than upTo is left for its own refresh")
}

func TestRetryPendingKeyRefreshes_FailureKeepsMarkerAndFails(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	tenantID := uuid.New()
	_, _ = markers.MarkPending(context.Background(), tenantID)
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("still down")

	result := retryPendingKeyRefreshes(context.Background(), markers, rp, 500, nil)

	assert.Equal(t, pendingRefreshResult{Failed: 1}, result)
	assert.True(t, markers.isPending(tenantID))
}

func TestRetryPendingKeyRefreshes_OffboardedTenantIsDroppedNotPagedForever(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	tenantID := uuid.New()
	_, _ = markers.MarkPending(context.Background(), tenantID)
	markers.hasPrin[tenantID] = false
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("404 realm not found")

	result := retryPendingKeyRefreshes(context.Background(), markers, rp, 500, nil)

	assert.Equal(t, pendingRefreshResult{Dropped: 1}, result)
	assert.False(t, markers.isPending(tenantID))
}

func TestRetryPendingKeyRefreshes_ListFailureFails(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	markers.listErr = errors.New("reconciler role missing")

	result := retryPendingKeyRefreshes(context.Background(), markers, newFakeRealmProvisionerClient(), 500, nil)

	assert.Equal(t, pendingRefreshResult{Failed: 1}, result)
}

func TestRetryPendingKeyRefreshes_SignalledRunDefers(t *testing.T) {
	markers := newFakeKeysRefreshMarkers()
	_, _ = markers.MarkPending(context.Background(), uuid.New())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rp := newFakeRealmProvisionerClient()

	result := retryPendingKeyRefreshes(ctx, markers, rp, 500, nil)

	assert.Equal(t, pendingRefreshResult{Deferred: 1}, result)
	assert.Empty(t, rp.refreshed)
}

// Finding: RunInTx may re-run the closure (40001/40P01). The row must be
// re-read inside it — a struct mutated by the rolled-back first attempt
// (Update bumps record_version) would fail the retry's optimistic lock.
func TestRevokeExpiredRotating_RetriedClosureRereadsRow(t *testing.T) {
	credentials := newFakeCredentialRepository()
	credentials.realistic = true
	secrets := newFakeSecretStore()
	tenantID := uuid.New()
	row := seedRotating(credentials, secrets, tenantID, 1)
	tx := &rollbackOnceTxRunner{credentials: credentials}

	_, err := revokeExpiredRotating(context.Background(), credentials, secrets, tx, newFakeKeysRefreshMarkers(), row)

	require.NoError(t, err)
	assert.Equal(t, 2, tx.attempts)
	cred, _ := credentials.FindByVersion(context.Background(), tenantID, row.PrincipalID, 1)
	assert.Equal(t, domain.CredentialStatusRevoked, cred.Status)
	assert.Equal(t, 2, cred.RecordVersion)
}
