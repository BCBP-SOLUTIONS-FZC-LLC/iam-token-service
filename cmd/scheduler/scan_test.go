package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

func TestRunCadenceScan_RotatesEveryDuePrincipal(t *testing.T) {
	t.Parallel()
	tenantA, principalA := uuid.New(), uuid.New()
	tenantB, principalB := uuid.New(), uuid.New()

	rr := &fakeReconcilerRepository{due: []port.DueForRotation{
		{TenantID: tenantA, PrincipalID: principalA, Version: 3},
		{TenantID: tenantB, PrincipalID: principalB, Version: 1},
	}}
	issuer := &fakeCredentialIssuer{}
	rp := newFakeRealmProvisionerClient()

	result := runCadenceScan(context.Background(), rr, issuer, newFakeKeysRefreshMarkers(), rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, 2, result.Rotated)
	assert.Equal(t, 0, result.Skipped)
	assert.Equal(t, 0, result.Failed)
	assert.ElementsMatch(t, []uuid.UUID{principalA, principalB}, issuer.calls)
	assert.Equal(t, 1, rp.refreshed[tenantA])
	assert.Equal(t, 1, rp.refreshed[tenantB])
}

func TestRunCadenceScan_SkipsAlreadyHandledPrincipals(t *testing.T) {
	t.Parallel()
	tenantRevoked, principalRevoked := uuid.New(), uuid.New()
	tenantInFlight, principalInFlight := uuid.New(), uuid.New()

	rr := &fakeReconcilerRepository{due: []port.DueForRotation{
		{TenantID: tenantRevoked, PrincipalID: principalRevoked, Version: 2},
		{TenantID: tenantInFlight, PrincipalID: principalInFlight, Version: 5},
	}}
	issuer := &fakeCredentialIssuer{
		errs: map[uuid.UUID]error{
			principalRevoked:  newRevokedError(),
			principalInFlight: newRotationInFlightError(),
		},
	}
	rp := newFakeRealmProvisionerClient()

	result := runCadenceScan(context.Background(), rr, issuer, newFakeKeysRefreshMarkers(), rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, 0, result.Rotated)
	assert.Equal(t, 2, result.Skipped)
	assert.Equal(t, 0, result.Failed)
}

func TestRunCadenceScan_FailsOnIssueOrRotateError(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 1}}}
	issuer := &fakeCredentialIssuer{errs: map[uuid.UUID]error{principalID: errors.New("openbao unavailable")}}
	rp := newFakeRealmProvisionerClient()

	result := runCadenceScan(context.Background(), rr, issuer, newFakeKeysRefreshMarkers(), rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, 0, result.Rotated)
	assert.Equal(t, 0, result.Skipped)
	assert.Equal(t, 1, result.Failed)
}

// TestRunCadenceScan_FailsWhenRPRelayFails covers the documented two-halves
// gap in rotateOneDue's doc comment: IssueOrRotate succeeds (this
// service's own record is already committed) but the RP-17 relay fails —
// classified Failed (page-worthy), never silently swallowed, since
// Keycloak is now out of sync with this service's record.
func TestRunCadenceScan_FailsWhenRPRelayFails(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 1}}}
	issuer := &fakeCredentialIssuer{}
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("realm provisioner unreachable")

	result := runCadenceScan(context.Background(), rr, issuer, newFakeKeysRefreshMarkers(), rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, 0, result.Rotated)
	assert.Equal(t, 0, result.Skipped)
	assert.Equal(t, 1, result.Failed)
}

func TestRunCadenceScan_ListDueForRotationErrorFailsTheRun(t *testing.T) {
	t.Parallel()
	rr := &fakeReconcilerRepository{err: errors.New("db unavailable")}
	issuer := &fakeCredentialIssuer{}
	rp := newFakeRealmProvisionerClient()

	result := runCadenceScan(context.Background(), rr, issuer, newFakeKeysRefreshMarkers(), rp, domain.DefaultOverlapSeconds, 500, nil)

	require.Equal(t, cadenceScanResult{Failed: 1}, result, "an enumeration failure must fail the Job, not look like an empty scan")
	assert.Empty(t, issuer.calls)
}

func TestRunCadenceScan_UsesConfiguredOverlap(t *testing.T) {
	t.Parallel()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1}}}
	issuer := &fakeCredentialIssuer{}

	runCadenceScan(context.Background(), rr, issuer, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), 120, 500, nil)

	assert.Equal(t, []int{120}, issuer.overlap, "ROTATION_DEFAULT_OVERLAP_SECONDS reaches IssueOrRotate")
}

// The scan rotates exactly the version it found due: IssueOrRotate gets
// ExpectActiveVersion = row.Version, so an operator rotation since the
// enumeration is refused rather than rotated again.
func TestRunCadenceScan_PassesExpectActiveVersion(t *testing.T) {
	t.Parallel()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{
		{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 7},
		{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 2},
	}}
	issuer := &fakeCredentialIssuer{}

	runCadenceScan(context.Background(), rr, issuer, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, []int{7, 2}, issuer.expect)
}

// optimistic_lock_conflict (the active version moved since enumeration) is
// a benign race: Skipped, not Failed, no RP-17 for a tenant where nothing
// rotated, and the scan's own fresh intent marker is withdrawn.
func TestRunCadenceScan_OptimisticLockConflictIsSkippedWithoutRP17(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 4}}}
	issuer := &fakeCredentialIssuer{errs: map[uuid.UUID]error{
		principalID: domain.NewError(domain.ErrOptimisticLockConflict, "the active credential version changed"),
	}}
	markers := newFakeKeysRefreshMarkers()
	rp := newFakeRealmProvisionerClient()

	result := runCadenceScan(context.Background(), rr, issuer, markers, rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, cadenceScanResult{Skipped: 1}, result)
	assert.Empty(t, rp.order, "no RP-17 for a tenant that had no rotation")
	assert.False(t, markers.isPending(tenantID), "the unused fresh intent marker is withdrawn")
}

// A marker that was already pending (an earlier refresh still owed) is not
// withdrawn just because this run rotated nothing for the tenant.
func TestRunCadenceScan_SkippedTenantKeepsEarlierPendingMarker(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 4}}}
	issuer := &fakeCredentialIssuer{errs: map[uuid.UUID]error{principalID: newRevokedError()}}
	markers := newFakeKeysRefreshMarkers()
	_, _ = markers.MarkPending(context.Background(), tenantID) // owed from an earlier run

	result := runCadenceScan(context.Background(), rr, issuer, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, cadenceScanResult{Skipped: 1}, result)
	assert.True(t, markers.isPending(tenantID))
}

// A generic IssueOrRotate error may still have committed (e.g. a lost
// COMMIT acknowledgement): the intent marker must stay for the next run.
func TestRunCadenceScan_AmbiguousFailureKeepsMarker(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 1}}}
	issuer := &fakeCredentialIssuer{errs: map[uuid.UUID]error{principalID: errors.New("connection reset during commit")}}
	markers := newFakeKeysRefreshMarkers()

	result := runCadenceScan(context.Background(), rr, issuer, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, 1, result.Failed)
	assert.True(t, markers.isPending(tenantID))
}

// Intent-first: the marker is already on record when IssueOrRotate runs.
func TestRunCadenceScan_MarkerWrittenBeforeIssueOrRotate(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 1}}}
	markers := newFakeKeysRefreshMarkers()
	var pendingAtIssue bool
	issuer := &fakeCredentialIssuer{onIssue: func(uuid.UUID) { pendingAtIssue = markers.isPending(tenantID) }}

	result := runCadenceScan(context.Background(), rr, issuer, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.True(t, pendingAtIssue, "the marker must commit before the rotation can")
	assert.Equal(t, 1, result.Rotated)
	assert.False(t, markers.isPending(tenantID), "cleared once RP-17 succeeded")
}

// A failed marker write means the rotation is not attempted at all — a
// rotation without a durable refresh record could lose its RP-17.
func TestRunCadenceScan_MarkerWriteFailureSkipsRotation(t *testing.T) {
	t.Parallel()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1}}}
	markers := newFakeKeysRefreshMarkers()
	markers.markErr = errors.New("db unavailable")
	issuer := &fakeCredentialIssuer{}

	result := runCadenceScan(context.Background(), rr, issuer, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, cadenceScanResult{Failed: 1}, result)
	assert.Empty(t, issuer.calls)
}

// RP-17 failure keeps the marker; the next run's retry pass refreshes and
// clears it.
func TestRunCadenceScan_RP17FailureKeepsMarkerForNextRun(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: uuid.New(), Version: 1}}}
	markers := newFakeKeysRefreshMarkers()
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("realm provisioner unreachable")

	result := runCadenceScan(context.Background(), rr, &fakeCredentialIssuer{}, markers, rp, domain.DefaultOverlapSeconds, 500, nil)
	require.Equal(t, 1, result.Failed)
	require.True(t, markers.isPending(tenantID))

	delete(rp.errs, tenantID)
	retry := retryPendingKeyRefreshes(context.Background(), markers, rp, 500, nil)

	assert.Equal(t, pendingRefreshResult{Refreshed: 1}, retry)
	assert.False(t, markers.isPending(tenantID))
}

// RP-17 must run on a context the run's cancellation cannot reach once
// IssueOrRotate has committed.
func TestRunCadenceScan_RP17RunsOnUncancelledContextAfterCommit(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: uuid.New(), Version: 1}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	issuer := &fakeCredentialIssuer{onIssue: func(uuid.UUID) { cancel() }} // SIGTERM lands during the rotation
	rp := newFakeRealmProvisionerClient()

	result := runCadenceScan(ctx, rr, issuer, newFakeKeysRefreshMarkers(), rp, domain.DefaultOverlapSeconds, 500, nil)

	require.Equal(t, []uuid.UUID{tenantID}, rp.order)
	assert.NoError(t, rp.ctxErrs[0], "RP-17 must not inherit the run context's cancellation")
	assert.Equal(t, 1, result.Rotated)
}

// Grouped by tenant in first-appearance order, RP-17 inline per tenant;
// a SIGTERM during tenant A lets A finish (with its RP-17) and defers B.
func TestRunCadenceScan_GroupsByTenantAndSignalStopsNewTenants(t *testing.T) {
	t.Parallel()
	tenantA, tenantB := uuid.New(), uuid.New()
	pA1, pB, pA2 := uuid.New(), uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{
		{TenantID: tenantA, PrincipalID: pA1, Version: 1},
		{TenantID: tenantB, PrincipalID: pB, Version: 1},
		{TenantID: tenantA, PrincipalID: pA2, Version: 1},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	issuer := &fakeCredentialIssuer{}
	rp := newFakeRealmProvisionerClient()
	rp.onRefresh = func(uuid.UUID) { cancel() }

	result := runCadenceScan(ctx, rr, issuer, newFakeKeysRefreshMarkers(), rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, []uuid.UUID{pA1, pA2}, issuer.calls, "both of A's rows run before B's")
	assert.Equal(t, []uuid.UUID{tenantA}, rp.order)
	assert.Equal(t, cadenceScanResult{Rotated: 2, Deferred: 1}, result)
}

// A tenant only starts with room for a rotation AND its RP-17 call.
func TestRunCadenceScan_TenantNeedsRoomForItsRP17(t *testing.T) {
	t.Parallel()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1}}}
	ctx, cancel := context.WithTimeout(context.Background(), rowReserve+rp17Timeout/2)
	defer cancel()
	issuer := &fakeCredentialIssuer{}

	result := runCadenceScan(ctx, rr, issuer, newFakeKeysRefreshMarkers(), newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, cadenceScanResult{Deferred: 1}, result)
	assert.Empty(t, issuer.calls)
}

// sweepMarkRefreshClear simulates a concurrent cmd/rotator sweep for the
// same tenant: it marks in its revoke transaction, its RP-17 succeeds, and
// it clears up to its own mark — which also covers any older marker.
func sweepMarkRefreshClear(t *testing.T, markers *fakeKeysRefreshMarkers, tenantID uuid.UUID) {
	t.Helper()
	mark, err := markers.MarkPending(context.Background(), tenantID)
	require.NoError(t, err)
	require.NoError(t, markers.Clear(context.Background(), tenantID, mark.RequestedAt))
}

// Regression (finding: a concurrent sweep deleted the scheduler's intent
// marker): the sweep marks, refreshes and clears between the scheduler's
// intent write and its commit; the scheduler's RP-17 then fails. The
// committed re-mark after IssueOrRotate must leave a marker for the next
// run — next_rotation_at has already advanced, nothing else retries it.
func TestRunCadenceScan_ConcurrentSweepClearDoesNotLoseRefresh(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 1}}}
	markers := newFakeKeysRefreshMarkers()
	issuer := &fakeCredentialIssuer{onIssue: func(uuid.UUID) { sweepMarkRefreshClear(t, markers, tenantID) }}
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("realm provisioner unreachable")

	result := runCadenceScan(context.Background(), rr, issuer, markers, rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, 1, result.Failed)
	require.True(t, markers.isPending(tenantID), "the refresh owed for the committed rotation must survive the sweep's clear")
	assert.False(t, markers.isIntent(tenantID), "it is a committed marker: the next run retries it at once")
}

// The same race one step later: the sweep clears after the scheduler's
// committed re-mark but before the scheduler's RP-17 fails. The re-mark on
// RP-17 failure restores the marker.
func TestRunCadenceScan_RP17FailureRemarksAfterConcurrentClear(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: uuid.New(), Version: 1}}}
	markers := newFakeKeysRefreshMarkers()
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("realm provisioner unreachable")
	rp.onRefresh = func(id uuid.UUID) { sweepMarkRefreshClear(t, markers, id) }

	result := runCadenceScan(context.Background(), rr, &fakeCredentialIssuer{}, markers, rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, 1, result.Failed)
	assert.True(t, markers.isPending(tenantID))
	assert.False(t, markers.isIntent(tenantID))
}

// While IssueOrRotate runs the marker is an intent (invisible to the retry
// pass); once the rotation commits it is converted into a committed marker,
// so a failed RP-17 is retried by the very next run, with no age guard.
func TestRunCadenceScan_IntentConvertedToCommittedAfterRotation(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: uuid.New(), Version: 1}}}
	markers := newFakeKeysRefreshMarkers()
	var intentAtIssue bool
	issuer := &fakeCredentialIssuer{onIssue: func(uuid.UUID) { intentAtIssue = markers.isIntent(tenantID) }}
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("realm provisioner unreachable")

	runCadenceScan(context.Background(), rr, issuer, markers, rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.True(t, intentAtIssue, "the pre-rotation marker is an intent")
	pending, err := markers.ListPending(context.Background(), 500)
	require.NoError(t, err)
	require.Len(t, pending, 1, "the committed marker is retryable at once")
	assert.Equal(t, tenantID, pending[0].TenantID)
}

// An ambiguous IssueOrRotate error may have committed: the intent is
// converted into a committed marker too.
func TestRunCadenceScan_AmbiguousFailureConvertsIntentToCommitted(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 1}}}
	issuer := &fakeCredentialIssuer{errs: map[uuid.UUID]error{principalID: errors.New("connection reset during commit")}}
	markers := newFakeKeysRefreshMarkers()

	runCadenceScan(context.Background(), rr, issuer, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.True(t, markers.isPending(tenantID))
	assert.False(t, markers.isIntent(tenantID))
	assert.Equal(t, 1, markers.commitMarks)
}

// A refused rotation (errAlreadyHandled) committed nothing: no committed
// re-mark, and the scan's own fresh intent is withdrawn.
func TestRunCadenceScan_AlreadyHandledDoesNotRemark(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 1}}}
	issuer := &fakeCredentialIssuer{errs: map[uuid.UUID]error{principalID: newRotationInFlightError()}}
	markers := newFakeKeysRefreshMarkers()

	runCadenceScan(context.Background(), rr, issuer, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.Zero(t, markers.commitMarks)
	assert.False(t, markers.isPending(tenantID))
}

// A failed committed re-mark is not fatal: the intent marker stays and is
// retried once intent_until passes.
func TestRunCadenceScan_RemarkFailureLeavesIntentForLaterRetry(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: uuid.New(), Version: 1}}}
	markers := newFakeKeysRefreshMarkers()
	markers.commitMarkErr = errors.New("db blip")
	rp := newFakeRealmProvisionerClient()
	rp.errs[tenantID] = errors.New("realm provisioner unreachable")

	result := runCadenceScan(context.Background(), rr, &fakeCredentialIssuer{}, markers, rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, 1, result.Failed)
	require.True(t, markers.isPending(tenantID))
	require.True(t, markers.isIntent(tenantID))
	pending, _ := markers.ListPending(context.Background(), 500)
	assert.Empty(t, pending, "a live intent waits for its writer")

	markers.expireIntents()
	delete(rp.errs, tenantID)
	retry := retryPendingKeyRefreshes(context.Background(), markers, rp, 500, nil)
	assert.Equal(t, pendingRefreshResult{Refreshed: 1}, retry)
	assert.False(t, markers.isPending(tenantID))
}

// When the re-mark fails but RP-17 succeeds, the refresh landed after the
// commit: the intent marker is cleared and the rotation counts as Rotated.
func TestRunCadenceScan_RemarkFailureWithRP17SuccessClears(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: uuid.New(), Version: 1}}}
	markers := newFakeKeysRefreshMarkers()
	markers.commitMarkErr = errors.New("db blip")

	result := runCadenceScan(context.Background(), rr, &fakeCredentialIssuer{}, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, cadenceScanResult{Rotated: 1}, result)
	assert.False(t, markers.isPending(tenantID))
}

// Regression: an offboarding cascade that soft-deleted the principal since
// enumeration surfaces as principal_not_found — a benign race, Skipped, not
// a page.
func TestRunCadenceScan_PrincipalNotFoundIsSkipped(t *testing.T) {
	t.Parallel()
	tenantID, principalID := uuid.New(), uuid.New()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: tenantID, PrincipalID: principalID, Version: 1}}}
	issuer := &fakeCredentialIssuer{errs: map[uuid.UUID]error{
		principalID: domain.NewError(domain.ErrPrincipalNotFound, "principal not found"),
	}}
	markers := newFakeKeysRefreshMarkers()
	rp := newFakeRealmProvisionerClient()

	result := runCadenceScan(context.Background(), rr, issuer, markers, rp, domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, cadenceScanResult{Skipped: 1}, result)
	assert.Empty(t, rp.order)
	assert.False(t, markers.isPending(tenantID), "the unused fresh intent marker is withdrawn")
}

// The intent outlives its writer's whole run: the 4-minute floor, stretched
// when the configured run is longer.
func TestIntentTTLFor(t *testing.T) {
	t.Parallel()
	assert.Equal(t, intentTTL, intentTTLFor(context.Background()))

	short, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	assert.Equal(t, intentTTL, intentTTLFor(short), "the default run fits under the floor")

	long, cancel2 := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel2()
	assert.Greater(t, intentTTLFor(long), 10*time.Minute+rp17Timeout)
}

// The scan passes intentTTLFor's value to MarkIntent.
func TestRunCadenceScan_MarksIntentWithTTL(t *testing.T) {
	t.Parallel()
	rr := &fakeReconcilerRepository{due: []port.DueForRotation{{TenantID: uuid.New(), PrincipalID: uuid.New(), Version: 1}}}
	markers := newFakeKeysRefreshMarkers()

	runCadenceScan(context.Background(), rr, &fakeCredentialIssuer{}, markers, newFakeRealmProvisionerClient(), domain.DefaultOverlapSeconds, 500, nil)

	assert.Equal(t, []time.Duration{intentTTL}, markers.intentTTLs)
}
