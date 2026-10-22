package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// TS-7 — RevokePrincipal (§5.4).

// seedLiveHistory gives p three versions — v1 revoked (TS-2), v2 rotating,
// v3 active — plus orphaned material under the client prefix that no row
// claims (a failed TS-1's write). Returns the orphan's path; the event log
// is cleared so a test sees only TS-7's own events.
func seedLiveHistory(t *testing.T, svc *service.CredentialService, secrets *fakeSecretStore, events *fakeEventPublisher, p *domain.ServiceAccountPrincipal) string {
	t.Helper()
	ctx := context.Background()
	issue := func() {
		_, err := svc.IssueOrRotate(ctx, p.TenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, domain.SystemPrincipalID)
		require.NoError(t, err)
	}
	issue()
	issue()
	_, err := svc.Revoke(ctx, p.TenantID, p.ID, 1, domain.SystemPrincipalID)
	require.NoError(t, err)
	issue()
	orphan := domain.OpenBaoPathFor(p.TenantID, p.KeycloakClientID, 9)
	require.NoError(t, secrets.Write(ctx, orphan, "orphan"))
	events.events = nil
	secrets.deleted = nil
	return orphan
}

func credentialStatuses(t *testing.T, credentials *fakeCredentialRepository, p *domain.ServiceAccountPrincipal) map[int]domain.CredentialStatus {
	t.Helper()
	creds, err := credentials.ListByPrincipal(context.Background(), p.TenantID, p.ID)
	require.NoError(t, err)
	out := map[int]domain.CredentialStatus{}
	for _, c := range creds {
		out[c.Version] = c.Status
	}
	return out
}

func eventTypes(events *fakeEventPublisher) []string {
	out := make([]string, 0, len(events.events))
	for _, e := range events.events {
		out = append(out, e.Type)
	}
	return out
}

func TestRevokePrincipal_RevokesEverything(t *testing.T) {
	svc, principals, credentials, secrets, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	orphan := seedLiveHistory(t, svc, secrets, events, p)

	res, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)

	assert.Equal(t, p.ID, res.PrincipalID)
	assert.Equal(t, tenantID, res.TenantID)
	assert.Equal(t, p.KeycloakClientID, res.KeycloakClientID)
	assert.Equal(t, domain.PrincipalStatusRevoked, res.Status)
	assert.Equal(t, 2, res.RecordVersion, "record_version bumped by the guarded UpdateStatus")
	assert.False(t, res.RevokedAt.IsZero())
	assert.Equal(t, []int{1, 2, 3}, res.Versions)
	assert.Equal(t, 2, res.NewlyRevoked, "v1 was already revoked")

	stored, err := principals.FindByID(ctx, tenantID, p.ID)
	require.NoError(t, err)
	assert.True(t, stored.IsRevoked())

	assert.Equal(t, map[int]domain.CredentialStatus{
		1: domain.CredentialStatusRevoked, 2: domain.CredentialStatusRevoked, 3: domain.CredentialStatusRevoked,
	}, credentialStatuses(t, credentials, p))
	creds, err := credentials.ListByPrincipal(ctx, tenantID, p.ID)
	require.NoError(t, err)
	for _, c := range creds {
		assert.Nil(t, c.NextRotationAt, "v%d: rotation unscheduled", c.Version)
		assert.Nil(t, c.RotationCadenceDays, "v%d", c.Version)
		assert.NotNil(t, c.RevokedAt, "v%d", c.Version)
	}

	assert.Empty(t, secrets.data, "every row's material and the orphan under the client prefix are gone")
	assert.Contains(t, secrets.deleted, orphan)

	assert.Equal(t, []string{
		domain.EventServiceAccountCredentialRevoked, domain.EventServiceAccountCredentialRevoked,
		domain.EventServiceAccountRevoked,
	}, eventTypes(events), "one per newly revoked version, then the principal-level event the purge emits")
	last := events.events[2].Data.(domain.ServiceAccountRevokedPayload)
	assert.Equal(t, domain.ServiceAccountRevokedPayload{
		TenantID: tenantID, PrincipalID: p.ID, ActorID: domain.SystemPrincipalID,
		RevokedAt: res.RevokedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}, last)

	// A revoked principal is terminal.
	_, err = svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	requireDomainCode(t, err, domain.ErrPrincipalRevoked)
}

func TestRevokePrincipal_IdempotentRepeatReturnsSameBody(t *testing.T) {
	svc, principals, _, secrets, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	seedLiveHistory(t, svc, secrets, events, p)

	first, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	emitted := len(events.events)
	deleted := len(secrets.deleted)

	again, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Zero(t, again.NewlyRevoked)
	again.NewlyRevoked = first.NewlyRevoked
	assert.Equal(t, first, again, "the wire body is identical")
	assert.Len(t, events.events, emitted, "no new events")
	assert.Len(t, secrets.deleted, deleted, "no OpenBao calls")
}

func TestRevokePrincipal_NoCredentials(t *testing.T) {
	svc, principals, _, _, events := newTestCredentialService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	res, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, domain.PrincipalStatusRevoked, res.Status)
	assert.Empty(t, res.Versions)
	assert.Equal(t, []string{domain.EventServiceAccountRevoked}, eventTypes(events))
	assert.Equal(t, p.ID, res.PrincipalID)
}

func TestRevokePrincipal_NotFound(t *testing.T) {
	svc, principals, _, _, events := newTestCredentialService(t)
	seedPrincipal(t, principals, uuid.New()) // another tenant's principal is invisible

	_, err := svc.RevokePrincipal(context.Background(), uuid.New(), domain.SystemPrincipalID)
	requireDomainCode(t, err, domain.ErrPrincipalNotFound)
	assert.Empty(t, events.events)
}

// The principal lock is held by an in-flight rotation past PG_LOCK_TIMEOUT:
// 409 rotation_in_flight with §17's active_rotation_id, nothing touched.
func TestRevokePrincipal_RotationInFlight(t *testing.T) {
	svc, principals, credentials, secrets, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	seedLiveHistory(t, svc, secrets, events, p)
	before := credentialStatuses(t, credentials, p)
	principals.forceLockErr = domain.NewError(domain.ErrRotationInFlight, "lock wait timeout")

	_, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	requireDomainCode(t, err, domain.ErrRotationInFlight)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	active, err := credentials.FindActive(ctx, tenantID, p.ID)
	require.NoError(t, err)
	assert.Equal(t, *active.RotationID, de.Details["active_rotation_id"])

	assert.Equal(t, before, credentialStatuses(t, credentials, p))
	assert.Empty(t, events.events)
	assert.Empty(t, secrets.deleted)
	stored, _ := principals.FindByID(ctx, tenantID, p.ID)
	assert.False(t, stored.IsRevoked())
}

// An OpenBao delete failure surfaces as 502 secret_store_unavailable before
// any Postgres write or event; the retry converges.
func TestRevokePrincipal_OpenBaoFailureThenRetryConverges(t *testing.T) {
	svc, principals, credentials, secrets, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	seedLiveHistory(t, svc, secrets, events, p)
	before := credentialStatuses(t, credentials, p)
	v2 := domain.OpenBaoPathFor(tenantID, p.KeycloakClientID, 2)
	secrets.deleteErrFor = map[string]error{v2: domain.NewError(domain.ErrSecretStoreUnavailable, "openbao down")}

	_, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	requireDomainCode(t, err, domain.ErrSecretStoreUnavailable)
	assert.Equal(t, before, credentialStatuses(t, credentials, p), "no row changed")
	stored, _ := principals.FindByID(ctx, tenantID, p.ID)
	assert.False(t, stored.IsRevoked(), "principal still active")
	assert.Equal(t, 1, stored.RecordVersion)
	assert.Empty(t, events.events, "no event for a failed revoke")
	assert.Contains(t, secrets.data, v2)

	secrets.deleteErrFor = nil
	res, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, domain.PrincipalStatusRevoked, res.Status)
	assert.Equal(t, 2, res.NewlyRevoked)
	assert.Empty(t, secrets.data)
}

func TestRevokePrincipal_SubtreeListFailure(t *testing.T) {
	svc, principals, _, secrets, events := newTestCredentialService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	seedLiveHistory(t, svc, secrets, events, p)
	boom := domain.NewError(domain.ErrSecretStoreUnavailable, "list failed")
	secrets.listErrFor = map[string]error{domain.OpenBaoPathPrefixFor(tenantID, p.KeycloakClientID): boom}

	_, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
	require.ErrorIs(t, err, boom)
	assert.Empty(t, events.events)
}

// A concurrent TS-7 committed while this one waited for the lock: success,
// the winner's result, no second set of events.
func TestRevokePrincipal_ConcurrentRevokeWonWhileWaitingForLock(t *testing.T) {
	svc, principals, _, secrets, events := newTestCredentialService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	principals.onLock = func(f *fakePrincipalRepository) {
		stored := f.byID[principalKey{tenantID, p.ID}]
		stored.Status = domain.PrincipalStatusRevoked
		stored.RecordVersion = 2
	}

	res, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, domain.PrincipalStatusRevoked, res.Status)
	assert.Equal(t, 2, res.RecordVersion)
	assert.Zero(t, res.NewlyRevoked)
	assert.Empty(t, events.events)
	assert.Empty(t, secrets.deleted)
}

// cmd/rotator's sweep does not take the principal lock: it can bump a
// rotating row's record_version under TS-7. The 409 surfaces (the caller
// retries) unless the principal already reached `revoked`.
func TestRevokePrincipal_OptimisticLockConflict(t *testing.T) {
	t.Run("surfaces as 409 while the principal is still active", func(t *testing.T) {
		svc, principals, credentials, secrets, events := newTestCredentialService(t)
		tenantID := uuid.New()
		p := seedPrincipal(t, principals, tenantID)
		seedLiveHistory(t, svc, secrets, events, p)
		credentials.raceOnUpdate = func(byID map[uuid.UUID]*domain.Credential) {
			for _, c := range byID {
				c.RecordVersion++
			}
		}
		_, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
		requireDomainCode(t, err, domain.ErrOptimisticLockConflict)
	})

	t.Run("is success when the principal was revoked meanwhile", func(t *testing.T) {
		svc, principals, credentials, secrets, events := newTestCredentialService(t)
		tenantID := uuid.New()
		p := seedPrincipal(t, principals, tenantID)
		seedLiveHistory(t, svc, secrets, events, p)
		credentials.raceOnUpdate = func(byID map[uuid.UUID]*domain.Credential) {
			for _, c := range byID {
				c.RecordVersion++
			}
			principals.byID[principalKey{tenantID, p.ID}].Status = domain.PrincipalStatusRevoked
		}
		res, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
		require.NoError(t, err)
		assert.Equal(t, domain.PrincipalStatusRevoked, res.Status)
		assert.Zero(t, res.NewlyRevoked)
	})

	t.Run("principal record_version conflict surfaces as 409", func(t *testing.T) {
		svc, principals, _, _, _ := newTestCredentialService(t)
		tenantID := uuid.New()
		seedPrincipal(t, principals, tenantID)
		principals.forceUpdateStatusErr = domain.NewError(domain.ErrOptimisticLockConflict, "stale")
		_, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
		requireDomainCode(t, err, domain.ErrOptimisticLockConflict)
	})
}

func TestRevokePrincipal_RepositoryErrors(t *testing.T) {
	boom := errors.New("db down")

	t.Run("find", func(t *testing.T) {
		svc, principals, _, _, _ := newTestCredentialService(t)
		principals.forceFindByIDErr = boom
		_, err := svc.RevokePrincipal(context.Background(), uuid.New(), domain.SystemPrincipalID)
		require.ErrorIs(t, err, boom)
	})
	t.Run("list credentials", func(t *testing.T) {
		svc, principals, credentials, _, _ := newTestCredentialService(t)
		tenantID := uuid.New()
		seedPrincipal(t, principals, tenantID)
		credentials.listByPrincipalErr = boom
		_, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
		require.ErrorIs(t, err, boom)
	})
	t.Run("list credentials for an already-revoked principal", func(t *testing.T) {
		svc, principals, credentials, _, _ := newTestCredentialService(t)
		tenantID := uuid.New()
		p := seedPrincipal(t, principals, tenantID)
		p.Status = domain.PrincipalStatusRevoked
		credentials.listByPrincipalErr = boom
		_, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
		require.ErrorIs(t, err, boom)
	})
	t.Run("credential update", func(t *testing.T) {
		svc, principals, credentials, secrets, events := newTestCredentialService(t)
		tenantID := uuid.New()
		p := seedPrincipal(t, principals, tenantID)
		seedLiveHistory(t, svc, secrets, events, p)
		credentials.forceUpdateErr = boom
		_, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
		require.ErrorIs(t, err, boom)
	})
}

func TestRevokePrincipal_NoEventPublisherOnContextStillSucceeds(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	svc := service.NewCredentialService(principals, credentials, newFakeSecretStore(), &fakeTxRunner{}, nil,
		func() (string, error) { return uuid.NewString(), nil })
	tenantID := uuid.New()
	seedPrincipal(t, principals, tenantID)

	res, err := svc.RevokePrincipal(context.Background(), tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, domain.PrincipalStatusRevoked, res.Status)
}

// TS-2 shares markCredentialRevoked with TS-7: revoking the active version
// clears its cadence too, so cmd/scheduler never picks it up.
func TestRevoke_ClearsCadenceOnTheRevokedRow(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	issued, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)

	_, err = svc.Revoke(ctx, tenantID, p.ID, issued.Version, domain.SystemPrincipalID)
	require.NoError(t, err)
	c, err := credentials.FindByVersion(ctx, tenantID, p.ID, issued.Version)
	require.NoError(t, err)
	assert.Nil(t, c.NextRotationAt)
	assert.Nil(t, c.RotationCadenceDays)
}

func requireDomainCode(t *testing.T, err error, code domain.ErrorCode) {
	t.Helper()
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	require.Equal(t, code, de.Code, de.Message)
}
