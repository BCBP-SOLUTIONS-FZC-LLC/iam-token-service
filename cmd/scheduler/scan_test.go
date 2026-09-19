package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

	result := runCadenceScan(context.Background(), rr, issuer, rp, nil)

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

	result := runCadenceScan(context.Background(), rr, issuer, rp, nil)

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

	result := runCadenceScan(context.Background(), rr, issuer, rp, nil)

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

	result := runCadenceScan(context.Background(), rr, issuer, rp, nil)

	assert.Equal(t, 0, result.Rotated)
	assert.Equal(t, 0, result.Skipped)
	assert.Equal(t, 1, result.Failed)
}

func TestRunCadenceScan_ListDueForRotationErrorReturnsEmptyResult(t *testing.T) {
	t.Parallel()
	rr := &fakeReconcilerRepository{err: errors.New("db unavailable")}
	issuer := &fakeCredentialIssuer{}
	rp := newFakeRealmProvisionerClient()

	result := runCadenceScan(context.Background(), rr, issuer, rp, nil)

	require.Equal(t, cadenceScanResult{}, result)
}
