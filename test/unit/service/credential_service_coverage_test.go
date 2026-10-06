package service_test

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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// ─────────────────────────────────────────────────────────────────────────
// Under-the-lock re-check branches of TS-1/TS-2. The service reads several
// rows twice — once as a cheap pre-check, once again under the principal
// lock — and the second read can disagree with the first when a concurrent
// writer committed in between. These wrappers script the second read so
// the race outcome is deterministic.
// ─────────────────────────────────────────────────────────────────────────

// revokedOnLockPrincipals reports the principal as revoked only from
// LockForUpdate: a revocation that committed while this call waited for
// the lock.
type revokedOnLockPrincipals struct {
	*fakePrincipalRepository
}

func (r *revokedOnLockPrincipals) LockForUpdate(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	p, err := r.fakePrincipalRepository.LockForUpdate(ctx, tenantID, principalID)
	if err != nil {
		return nil, err
	}
	p.Status = domain.PrincipalStatusRevoked
	return p, nil
}

// readHook rewrites the result of the call'th (1-based) invocation of a
// repository read.
type readHook func(call int, c *domain.Credential, err error) (*domain.Credential, error)

// scriptedCredentialRepository lets a test rewrite FindByRotationID /
// FindByVersion results per call, and fail MaxVersion.
type scriptedCredentialRepository struct {
	*fakeCredentialRepository
	rotationIDCalls int
	onRotationID    readHook
	versionCalls    int
	onVersion       readHook
	maxVersionErr   error
}

func (s *scriptedCredentialRepository) FindByRotationID(ctx context.Context, tenantID, principalID, rotationID uuid.UUID) (*domain.Credential, error) {
	c, err := s.fakeCredentialRepository.FindByRotationID(ctx, tenantID, principalID, rotationID)
	s.rotationIDCalls++
	if s.onRotationID != nil {
		return s.onRotationID(s.rotationIDCalls, c, err)
	}
	return c, err
}

func (s *scriptedCredentialRepository) FindByVersion(ctx context.Context, tenantID, principalID uuid.UUID, version int) (*domain.Credential, error) {
	c, err := s.fakeCredentialRepository.FindByVersion(ctx, tenantID, principalID, version)
	s.versionCalls++
	if s.onVersion != nil {
		return s.onVersion(s.versionCalls, c, err)
	}
	return c, err
}

func (s *scriptedCredentialRepository) MaxVersion(ctx context.Context, tenantID, principalID uuid.UUID) (int, error) {
	if s.maxVersionErr != nil {
		return 0, s.maxVersionErr
	}
	return s.fakeCredentialRepository.MaxVersion(ctx, tenantID, principalID)
}

type coverageFixture struct {
	principals  *fakePrincipalRepository
	credentials *scriptedCredentialRepository
	secrets     *fakeSecretStore
	events      *fakeEventPublisher
	tenantID    uuid.UUID
	principalID uuid.UUID
}

func newCoverageFixture(t *testing.T) *coverageFixture {
	t.Helper()
	f := &coverageFixture{
		principals:  newFakePrincipalRepository(),
		credentials: &scriptedCredentialRepository{fakeCredentialRepository: newFakeCredentialRepository()},
		secrets:     newFakeSecretStore(),
		events:      &fakeEventPublisher{},
		tenantID:    uuid.New(),
	}
	f.principalID = seedPrincipal(t, f.principals, f.tenantID).ID
	return f
}

func (f *coverageFixture) service() *service.CredentialService {
	return f.serviceWith(f.principals)
}

func (f *coverageFixture) serviceWith(principals port.PrincipalRepository) *service.CredentialService {
	return service.NewCredentialService(principals, f.credentials, f.secrets, &fakeTxRunner{events: f.events}, nil,
		func() (string, error) { return uuid.NewString(), nil })
}

func (f *coverageFixture) issue(t *testing.T, svc *service.CredentialService, rotationID uuid.UUID, overlap int) *service.IssueOrRotateResult {
	t.Helper()
	res, err := svc.IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: rotationID, OverlapSeconds: overlap}, domain.SystemPrincipalID)
	require.NoError(t, err)
	return res
}

func (f *coverageFixture) rowCount() int {
	n := 0
	for _, c := range f.credentials.byID {
		if c.TenantID == f.tenantID && c.PrincipalID == f.principalID {
			n++
		}
	}
	return n
}

// The principal passed the unlocked pre-check but was revoked by the time
// the lock was granted: 409 principal_revoked, nothing written.
func TestCredentialService_IssueOrRotate_PrincipalRevokedUnderLock(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.serviceWith(&revokedOnLockPrincipals{f.principals})

	res, err := svc.IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	assert.Nil(t, res)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalRevoked, de.Code)
	assert.Zero(t, f.rowCount())
	assert.Empty(t, f.secrets.data, "no material may be written for a revoked principal")
	assert.Empty(t, f.events.events)
}

// The rotation_id re-check under the lock fails: the error propagates and
// nothing is written.
func TestCredentialService_IssueOrRotate_RotationIDRecheckUnderLockError(t *testing.T) {
	f := newCoverageFixture(t)
	boom := errors.New("db unavailable")
	f.credentials.onRotationID = func(call int, c *domain.Credential, err error) (*domain.Credential, error) {
		if call == 2 {
			return nil, boom
		}
		return c, err
	}

	_, err := f.service().IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 2, f.credentials.rotationIDCalls)
	assert.Zero(t, f.rowCount())
	assert.Empty(t, f.secrets.data)
}

// A concurrent call with the same rotation_id committed while this one
// waited for the lock: this call is a replay of that result (same version,
// same key), writes no new row and enqueues no event.
func TestCredentialService_IssueOrRotate_ConcurrentSameRotationIDReplaysUnderLock(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.service()
	rotationID := uuid.New()
	first := f.issue(t, svc, rotationID, 0)
	eventsBefore := len(f.events.events)

	// The pre-check misses the committed row (not visible yet); the re-check
	// under the lock sees it.
	f.credentials.rotationIDCalls = 0
	f.credentials.onRotationID = func(call int, c *domain.Credential, err error) (*domain.Credential, error) {
		if call == 1 {
			return nil, nil
		}
		return c, err
	}

	res, err := svc.IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.True(t, res.Replayed)
	assert.Equal(t, first.Version, res.Version)
	assert.Equal(t, first.Secret, res.Secret)
	assert.Equal(t, 2, f.credentials.rotationIDCalls)
	assert.Equal(t, 1, f.rowCount(), "a replay must not insert a row")
	assert.Len(t, f.events.events, eventsBefore, "a replay must not enqueue an event")
}

// MaxVersion fails under the lock: the error propagates, nothing written.
func TestCredentialService_IssueOrRotate_MaxVersionError(t *testing.T) {
	f := newCoverageFixture(t)
	boom := errors.New("db unavailable")
	f.credentials.maxVersionErr = boom

	_, err := f.service().IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.ErrorIs(t, err, boom)
	assert.Zero(t, f.rowCount())
	assert.Empty(t, f.secrets.data)
}

// expireOpenOverlaps cannot list the principal's credentials: the rotate
// fails and the active version is left untouched.
func TestCredentialService_IssueOrRotate_ExpireOverlapsListError(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.service()
	f.issue(t, svc, uuid.New(), 0)
	boom := errors.New("db unavailable")
	f.credentials.listByPrincipalErr = boom

	_, err := svc.IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.ErrorIs(t, err, boom)
	v1, err := f.credentials.FindByVersion(context.Background(), f.tenantID, f.principalID, 1)
	require.NoError(t, err)
	assert.Equal(t, domain.CredentialStatusActive, v1.Status)
	assert.Equal(t, 1, f.rowCount())
}

// expireOpenOverlaps fails ending an earlier still-open overlap (a third
// rotate inside the second's window): the error propagates and no new
// version is inserted.
func TestCredentialService_IssueOrRotate_ExpireOverlapsUpdateError(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.service()
	f.issue(t, svc, uuid.New(), 0)
	f.issue(t, svc, uuid.New(), 3600) // v1 -> rotating, overlap open for an hour
	boom := errors.New("db unavailable")
	f.credentials.forceUpdateErr = boom

	_, err := svc.IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 2, f.rowCount(), "v3 must not be inserted")
	v1, err := f.credentials.FindByVersion(context.Background(), f.tenantID, f.principalID, 1)
	require.NoError(t, err)
	require.NotNil(t, v1.ExpiresAt)
	assert.True(t, v1.ExpiresAt.After(time.Now()), "v1's overlap must still be open")
}

// TS-2: the re-read under the lock fails — the error propagates and the
// material is not deleted.
func TestCredentialService_Revoke_RereadUnderLockError(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.service()
	res := f.issue(t, svc, uuid.New(), 0)
	boom := errors.New("db unavailable")
	f.credentials.onVersion = func(call int, c *domain.Credential, err error) (*domain.Credential, error) {
		if call == 2 {
			return nil, boom
		}
		return c, err
	}

	_, err := svc.Revoke(context.Background(), f.tenantID, f.principalID, res.Version, domain.SystemPrincipalID)
	require.ErrorIs(t, err, boom)
	assert.Contains(t, f.secrets.data, res.OpenBaoPath, "material must survive a failed revoke")
}

// TS-2: the row vanished between the pre-read and the locked re-read (an
// offboarding cascade deleted it): 404, nothing deleted.
func TestCredentialService_Revoke_RowGoneUnderLock(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.service()
	res := f.issue(t, svc, uuid.New(), 0)
	f.credentials.onVersion = func(call int, c *domain.Credential, err error) (*domain.Credential, error) {
		if call == 2 {
			return nil, nil
		}
		return c, err
	}

	_, err := svc.Revoke(context.Background(), f.tenantID, f.principalID, res.Version, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalNotFound, de.Code)
	assert.Contains(t, f.secrets.data, res.OpenBaoPath)
}

// TS-2: another revoke won the race while this call waited for the lock —
// idempotent success with the winner's revoked_at, no second delete, no
// second event.
func TestCredentialService_Revoke_RevokedByConcurrentCallWhileWaitingForLock(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.service()
	res := f.issue(t, svc, uuid.New(), 0)
	eventsBefore := len(f.events.events)
	winnerAt := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	f.credentials.onVersion = func(call int, c *domain.Credential, err error) (*domain.Credential, error) {
		if call == 2 && c != nil {
			c.Status = domain.CredentialStatusRevoked
			c.RevokedAt = &winnerAt
		}
		return c, err
	}

	out, err := svc.Revoke(context.Background(), f.tenantID, f.principalID, res.Version, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, domain.CredentialStatusRevoked, out.Status)
	assert.Equal(t, res.Version, out.Version)
	assert.True(t, winnerAt.Equal(out.RevokedAt), "the winner's revoked_at is reported")
	assert.Contains(t, f.secrets.data, res.OpenBaoPath, "the loser must not delete again")
	assert.Len(t, f.events.events, eventsBefore, "the loser must not enqueue a second event")
}

// A rotation_in_flight that already carries details.active_rotation_id
// (e.g. the postgres adapter's uq_sac_one_active enrichment) is returned
// unchanged — never overwritten by the unlocked lookup.
func TestCredentialService_RotationInFlightWithActiveRotationIDIsNotReEnriched(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.service()
	f.issue(t, svc, uuid.New(), 0)
	adapterValue := uuid.New()
	orig := domain.NewError(domain.ErrRotationInFlight, "in flight").
		WithDetails(map[string]any{"active_rotation_id": adapterValue})
	f.principals.forceLockErr = orig

	_, err := svc.IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Same(t, orig, de)
	assert.Equal(t, adapterValue, de.Details["active_rotation_id"])
}

// Enrichment keeps any details the original rotation_in_flight already
// carried, alongside the added active_rotation_id.
func TestCredentialService_RotationInFlightEnrichmentPreservesExistingDetails(t *testing.T) {
	f := newCoverageFixture(t)
	svc := f.service()
	activeRotation := uuid.New()
	f.issue(t, svc, activeRotation, 0)
	f.principals.forceLockErr = domain.NewError(domain.ErrRotationInFlight, "in flight").
		WithDetails(map[string]any{"retry_after_seconds": 2})

	_, err := svc.IssueOrRotate(context.Background(), f.tenantID, f.principalID,
		service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
	assert.Equal(t, "in flight", de.Message)
	assert.Equal(t, 2, de.Details["retry_after_seconds"])
	assert.Equal(t, activeRotation, de.Details["active_rotation_id"])
}
