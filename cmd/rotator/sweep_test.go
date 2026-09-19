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

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeRealmProvisionerClient(), nil)

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

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeRealmProvisionerClient(), nil)

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

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeRealmProvisionerClient(), nil)

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

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeRealmProvisionerClient(), nil)

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

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, rp, nil)

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

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, rp, nil)

	assert.Equal(t, 0, result.Revoked)
	assert.Equal(t, 1, result.Failed)

	cred, err := credentials.FindByVersion(context.Background(), tenantID, principalID, 1)
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, domain.CredentialStatusRevoked, cred.Status, "the DB revoke is not rolled back on an RP-17 failure")
}

func TestRunOverlapSweep_ListFailureIsSafeNoOp(t *testing.T) {
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{}
	rr := &fakeReconcilerRepository{err: errors.New("db unavailable")}

	result := runOverlapSweep(context.Background(), rr, credentials, secrets, tx, newFakeRealmProvisionerClient(), nil)

	assert.Equal(t, sweepResult{}, result)
}
