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

// materialState builds a principal whose rows are all live, at the
// current client id's prefix. maxVersion is the highest committed version.
func materialState(tenantID, principalID uuid.UUID, maxVersion int, live ...int) port.PrincipalMaterialState {
	st := port.PrincipalMaterialState{
		TenantID: tenantID, PrincipalID: principalID, KeycloakClientID: domain.KeycloakClientPlatformAutomation,
		MaxVersion: maxVersion,
	}
	for _, v := range live {
		st.Credentials = append(st.Credentials, port.CredentialMaterial{
			Version: v, OpenBaoPath: domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, v), Live: true,
		})
	}
	return st
}

func withRevoked(st port.PrincipalMaterialState, versions ...int) port.PrincipalMaterialState {
	for _, v := range versions {
		st.Credentials = append(st.Credentials, port.CredentialMaterial{
			Version: v, OpenBaoPath: domain.OpenBaoPathFor(st.TenantID, st.KeycloakClientID, v), Live: false,
		})
	}
	return st
}

func prefixFor(tenantID uuid.UUID) string {
	return "iam/serviceaccount/" + tenantID.String() + "/" + domain.KeycloakClientPlatformAutomation
}

func TestOrphanReconciler_DeletesBelowMaxUncommittedVersion(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "v2", "v3"}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(tenantID, principalID, 3, 3), // only v3 is committed; v1,v2 are orphans below max
	}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, 2, result.OrphanDeleted)
	assert.Equal(t, 1, result.OK)
	assert.Equal(t, 0, result.MissingMaterial)
	assert.ElementsMatch(t, []string{
		domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1),
		domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 2),
	}, secrets.deleted)
}

// The old "version >= MaxVersion may be in flight" exclusion is replaced
// by a re-check under the principal lock TS-1 holds across its OpenBao
// write and commit: here a TS-1 commits v2 just before the reconciler gets
// the lock, so v2 is not an orphan and must survive.
func TestOrphanReconciler_LeavesVersionCommittedBeforeLockAcquired(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "v2"}
	credentials := newFakeCredentialRepository()
	lock := testLock(credentials)
	lock.principals.(*fakePrincipalLocker).onLock = func(uuid.UUID) {
		credentials.seed(&domain.Credential{TenantID: tenantID, PrincipalID: principalID, Version: 2,
			Status: domain.CredentialStatusActive, OpenBaoPath: domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 2)})
	}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(tenantID, principalID, 1, 1), // the snapshot predates v2's commit
	}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, nil)

	assert.Equal(t, 0, result.OrphanDeleted)
	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, 0, result.Failed)
	assert.Empty(t, secrets.deleted)
}

// The lock is what makes the delete safe: an uncommitted version at or
// above MaxVersion with no row under the lock is a crashed TS-1's orphan,
// and is now reclaimed instead of skipped forever.
func TestOrphanReconciler_DeletesUnclaimedVersionAtOrAboveMaxUnderLock(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "v2"}
	lock := testLock(nil)

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(tenantID, principalID, 1, 1),
	}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, nil)

	assert.Equal(t, 1, result.OrphanDeleted)
	assert.Equal(t, 1, result.OK)
	assert.Equal(t, []uuid.UUID{principalID}, lock.principals.(*fakePrincipalLocker).lockedBy, "the delete happens under the principal lock")
	assert.Equal(t, []string{domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 2)}, secrets.deleted)
}

// Regression: MaxVersion is 0 for a principal with no credential rows, and
// `version >= MaxVersion` used to skip every version it had.
func TestOrphanReconciler_ReclaimsMaterialOfPrincipalWithNoRows(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1"}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 0)}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, 1, result.OrphanDeleted)
	assert.Equal(t, []string{domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)}, secrets.deleted)
}

// A 55P03 lock timeout (domain.ErrRotationInFlight) means a TS-1 holds the
// lock right now: leave the candidate for the next run, not Failed.
func TestOrphanReconciler_LockTimeoutIsSkippedNotFailed(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "v2"}
	lock := testLock(nil)
	lock.principals.(*fakePrincipalLocker).errs = map[uuid.UUID]error{
		principalID: domain.NewError(domain.ErrRotationInFlight, "lock timeout"),
	}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, nil)

	assert.Equal(t, 0, result.Failed)
	assert.Equal(t, 1, result.Skipped)
	assert.Empty(t, secrets.deleted)
}

func TestOrphanReconciler_DefersPrincipalsWhenBudgetIsSpent(t *testing.T) {
	secrets := newFakeSecretStore()
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(uuid.New(), uuid.New(), 1, 1),
		materialState(uuid.New(), uuid.New(), 1, 1),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), rowReserve/2)
	defer cancel()

	result := runOrphanMaterialReconciler(ctx, rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, materialReconcileResult{Deferred: 2}, result, "out-of-budget principals are Deferred, never Failed")
}

func TestOrphanReconciler_SignalledRunDefersNotFails(t *testing.T) {
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(uuid.New(), uuid.New(), 1, 1)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // SIGTERM before the phase started

	result := runOrphanMaterialReconciler(ctx, rr, newFakeSecretStore(), testLock(nil), 0, nil)

	assert.Equal(t, materialReconcileResult{Deferred: 1}, result)
}

// The start offset rotates with the seed, so a budget that runs out does
// not always starve the same (high-id) principals.
func TestOrphanReconciler_StartOffsetRotatesWithSeed(t *testing.T) {
	var states []port.PrincipalMaterialState
	for i := 0; i < 3; i++ {
		states = append(states, materialState(uuid.New(), uuid.New(), 1, 1))
	}
	firstListed := func(seed int64) string {
		secrets := &recordingSecretStore{fakeSecretStore: newFakeSecretStore()}
		runOrphanMaterialReconciler(context.Background(), &fakeReconcilerRepository{states: states}, secrets, testLock(nil), seed, nil)
		return secrets.listed[0]
	}

	assert.Equal(t, prefixFor(states[0].TenantID), firstListed(0))
	assert.Equal(t, prefixFor(states[1].TenantID), firstListed(1))
	assert.Equal(t, prefixFor(states[2].TenantID), firstListed(5))
}

// recordingSecretStore records the prefixes List was called with, in order.
type recordingSecretStore struct {
	*fakeSecretStore
	listed []string
}

func (r *recordingSecretStore) List(ctx context.Context, prefix string) ([]string, error) {
	r.listed = append(r.listed, prefix)
	return r.fakeSecretStore.List(ctx, prefix)
}

// testLock builds an orphanLock over in-memory fakes; credentials may be
// nil for an empty repository.
func testLock(credentials *fakeCredentialRepository) orphanLock {
	if credentials == nil {
		credentials = newFakeCredentialRepository()
	}
	return orphanLock{tx: &fakeTxRunner{}, principals: &fakePrincipalLocker{}, credentials: credentials}
}

func TestOrphanReconciler_DetectsMissingMaterial(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	// v1 is committed but OpenBao has nothing at all under this prefix.
	secrets.byPrefix[prefixFor(tenantID)] = nil

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(tenantID, principalID, 1, 1),
	}}

	credentials := newFakeCredentialRepository()
	credentials.seed(liveCredential(tenantID, principalID, 1))

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(credentials), 0, nil)

	assert.Equal(t, 1, result.MissingMaterial)
	assert.Equal(t, 0, result.OrphanDeleted)
	assert.Equal(t, 0, result.OK)
}

func liveCredential(tenantID, principalID uuid.UUID, version int) *domain.Credential {
	return &domain.Credential{TenantID: tenantID, PrincipalID: principalID, Version: version,
		Status: domain.CredentialStatusActive, OpenBaoPath: domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, version)}
}

// Regression: TS-2 deletes material before it commits the revoke, under the
// principal lock. The unlocked List misses the path while the unlocked
// IsCredentialLive still says live; re-checking under the lock sees the
// committed revoke instead of paging missing_material.
func TestOrphanReconciler_RevokeInFlightIsNotMissing(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = nil // TS-2 already deleted v1's material
	credentials := newFakeCredentialRepository()
	cred := liveCredential(tenantID, principalID, 1)
	credentials.seed(cred)
	lock := testLock(credentials)
	// TS-2 commits while the reconciler waits for the lock.
	lock.principals.(*fakePrincipalLocker).onLock = func(uuid.UUID) { cred.Status = domain.CredentialStatusRevoked }

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, nil)

	assert.Equal(t, 0, result.MissingMaterial)
	assert.Equal(t, 0, result.Failed)
	assert.Equal(t, 1, result.OK)
	assert.Equal(t, []uuid.UUID{principalID}, lock.principals.(*fakePrincipalLocker).lockedBy)
}

// Material that is present by the time the lock is held (the unlocked List
// raced a write) is OK, not missing.
func TestOrphanReconciler_MaterialPresentUnderLockIsOK(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = nil
	credentials := newFakeCredentialRepository()
	credentials.seed(liveCredential(tenantID, principalID, 1))
	lock := testLock(credentials)
	lock.principals.(*fakePrincipalLocker).onLock = func(uuid.UUID) { secrets.byPrefix[prefixFor(tenantID)] = []string{"v1"} }

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, nil)

	assert.Equal(t, 0, result.MissingMaterial)
	assert.Equal(t, 1, result.OK)
}

// A busy principal lock (55P03 → ErrRotationInFlight: a TS-1/TS-2 holds it
// right now) leaves the missing-material check to the next run.
func TestOrphanReconciler_MissingCheckLockTimeoutIsSkipped(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = nil
	credentials := newFakeCredentialRepository()
	credentials.seed(liveCredential(tenantID, principalID, 1))
	lock := testLock(credentials)
	lock.principals.(*fakePrincipalLocker).errs = map[uuid.UUID]error{
		principalID: domain.NewError(domain.ErrRotationInFlight, "lock timeout"),
	}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, lock, 0, nil)

	assert.Equal(t, 0, result.MissingMaterial)
	assert.Equal(t, 0, result.Failed)
	assert.Equal(t, 1, result.Skipped)
}

// An OpenBao failure during the locked re-check is Failed, never reported
// as missing material.
func TestOrphanReconciler_MissingCheckListErrorFails(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	secrets := &failSecondListStore{fakeSecretStore: newFakeSecretStore()}
	credentials := newFakeCredentialRepository()
	credentials.seed(liveCredential(tenantID, principalID, 1))

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(credentials), 0, nil)

	assert.Equal(t, 0, result.MissingMaterial)
	assert.Equal(t, 1, result.Failed)
}

// failSecondListStore succeeds the first List (the unlocked scan) and fails
// every later one (the locked re-check).
type failSecondListStore struct {
	*fakeSecretStore
	calls int
}

func (f *failSecondListStore) List(ctx context.Context, prefix string) ([]string, error) {
	f.calls++
	if f.calls > 1 {
		return nil, errors.New("openbao down")
	}
	return f.fakeSecretStore.List(ctx, prefix)
}

// The orphan delete runs while the principal lock is held, so it must be
// bounded (inLockSecretTimeout) even when the run context has no deadline.
func TestOrphanReconciler_OrphanDeleteUnderLockIsBounded(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	secrets := &deadlineRecordingStore{fakeSecretStore: newFakeSecretStore()}
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "v2"}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	require.Equal(t, 1, result.OrphanDeleted)
	require.True(t, secrets.hadDeadline, "the in-lock delete must carry a deadline")
	assert.LessOrEqual(t, secrets.remaining, inLockSecretTimeout)
}

type deadlineRecordingStore struct {
	*fakeSecretStore
	hadDeadline bool
	remaining   time.Duration
}

func (d *deadlineRecordingStore) Delete(ctx context.Context, p string) error {
	var deadline time.Time
	deadline, d.hadDeadline = ctx.Deadline()
	d.remaining = time.Until(deadline)
	return d.fakeSecretStore.Delete(ctx, p)
}

// Regression: revoked versions have their material deleted by design
// (CUST-2) — they must never be reported missing (that paged and failed the
// rotator on every run after the first rotation).
func TestOrphanReconciler_RevokedVersionWithoutMaterialIsNotMissing(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v3"}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		withRevoked(materialState(tenantID, principalID, 3, 3), 1, 2),
	}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, 0, result.MissingMaterial)
	assert.Equal(t, 0, result.Failed)
	assert.Equal(t, 1, result.OK)
	assert.Empty(t, secrets.deleted)
}

func TestOrphanReconciler_DeletesSurvivingMaterialOfRevokedVersion(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "v2"}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		withRevoked(materialState(tenantID, principalID, 2, 2), 1),
	}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, 1, result.OrphanDeleted, "a revoked credential's material must not survive")
	assert.Equal(t, []string{domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)}, secrets.deleted)
}

// A re-mint that changed keycloak_client_id leaves older versions under the
// old prefix: they are reconciled there, not reported missing.
func TestOrphanReconciler_ScansPrefixOfEveryStoredPath(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	oldClient := domain.KeycloakClientPlatformAutomation
	newClient := domain.KeycloakClientPlatformAutomation + "-" + tenantID.String()
	secrets.byPrefix[domain.OpenBaoPathPrefixFor(tenantID, oldClient)] = []string{"v1", "v2"}
	secrets.byPrefix[domain.OpenBaoPathPrefixFor(tenantID, newClient)] = []string{"v3"}

	st := port.PrincipalMaterialState{
		TenantID: tenantID, PrincipalID: principalID, KeycloakClientID: newClient, MaxVersion: 3,
		Credentials: []port.CredentialMaterial{
			{Version: 2, OpenBaoPath: domain.OpenBaoPathFor(tenantID, oldClient, 2), Live: true},
			{Version: 3, OpenBaoPath: domain.OpenBaoPathFor(tenantID, newClient, 3), Live: true},
		},
	}
	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{st}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, 0, result.MissingMaterial)
	assert.Equal(t, 2, result.OK)
	assert.Equal(t, 1, result.OrphanDeleted, "v1 under the old prefix has no row and is below max")
	assert.Equal(t, []string{domain.OpenBaoPathFor(tenantID, oldClient, 1)}, secrets.deleted)
}

// A live row revoked between the snapshot and the OpenBao list is re-checked
// and not paged on.
func TestOrphanReconciler_RechecksBeforeReportingMissing(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = nil

	rr := &fakeReconcilerRepository{
		states:  []port.PrincipalMaterialState{materialState(tenantID, principalID, 1, 1)},
		notLive: map[uuid.UUID]bool{principalID: true},
	}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, 0, result.MissingMaterial)
}

func TestOrphanReconciler_ListFailureFailsTheRun(t *testing.T) {
	rr := &fakeReconcilerRepository{err: errors.New("reconciler role missing")}

	result := runOrphanMaterialReconciler(context.Background(), rr, newFakeSecretStore(), testLock(nil), 0, nil)

	assert.Equal(t, 1, result.Failed, "an enumeration failure must not look like a clean run")
}

func TestOrphanReconciler_NoPrincipalsIsNoOp(t *testing.T) {
	secrets := newFakeSecretStore()
	rr := &fakeReconcilerRepository{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, testLock(nil), 0, nil)

	assert.Equal(t, materialReconcileResult{}, result)
}

func TestParseVersionKey(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"v1", 1, true},
		{"v42/", 42, true},
		{"metadata", 0, false},
		{"v0", 0, false},
		{"vX", 0, false},
	}
	for _, c := range cases {
		got, ok := parseVersionKey(c.in)
		assert.Equal(t, c.wantOK, ok, "input %q", c.in)
		if c.wantOK {
			assert.Equal(t, c.want, got, "input %q", c.in)
		}
	}
}
