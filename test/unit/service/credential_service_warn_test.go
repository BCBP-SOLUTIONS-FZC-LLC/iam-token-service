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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// These tests target CredentialService.sweepExpiredRotating's two warn()
// branches — best-effort cleanup opportunistically run at the tail of
// issueOrRotate (§8.3), whose own failures must never fail the TS-1 call
// that triggered them, only get logged.

func TestCredentialService_SweepExpiredRotating_WarnsOnListFailure(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	events := &fakeEventPublisher{}
	log := &fakeLogger{}
	generate := func() (string, error) { return uuid.NewString(), nil }
	svc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{events: events}, log, generate)

	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	// The sweep runs only after a successful commit, so arm the list
	// failure just before the call it's meant to affect.
	credentials.listByPrincipalErr = errors.New("list boom")

	res, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	// The sweep failure must never fail the call that triggered it.
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Len(t, log.warnings, 1)
	assert.Equal(t, "sweep_expired_rotating: list failed", log.warnings[0].msg)
	assert.Equal(t, tenantID, log.warnings[0].fields["tenant_id"])
	assert.Equal(t, p.ID, log.warnings[0].fields["principal_id"])
	assert.Contains(t, log.warnings[0].fields["error"], "list boom")
}

func TestCredentialService_SweepExpiredRotating_WarnsOnRevokeFailure(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	events := &fakeEventPublisher{}
	log := &fakeLogger{}
	generate := func() (string, error) { return uuid.NewString(), nil }
	svc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{events: events}, log, generate)

	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	// Seed an already-expired `rotating` credential directly (white-box:
	// this test package already reaches into fakeCredentialRepository's
	// unexported map elsewhere via seedPrincipal's sibling helpers) so the
	// very next IssueOrRotate's opportunistic sweep finds it and attempts
	// to revoke it. Version 99 is arbitrary and deliberately far from the
	// version=1 the IssueOrRotate call below will Insert — a same-version
	// row (even a non-active one) would trip the fake's rotation_in_flight
	// conflict check, which isn't what this test is about.
	staleID := uuid.New()
	expiredAt := time.Now().Add(-time.Minute)
	credentials.byID[staleID] = &domain.Credential{
		ID: staleID, TenantID: tenantID, PrincipalID: p.ID, Version: 99,
		Status: domain.CredentialStatusRotating, OpenBaoPath: "iam/serviceaccount/stale/v99",
		ExpiresAt: &expiredAt, RecordVersion: 1,
	}
	secrets.deleteErr = errors.New("openbao unreachable")

	res, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	// The sweep failure must never fail the call that triggered it.
	require.NoError(t, err)
	require.NotNil(t, res)

	require.Len(t, log.warnings, 1)
	assert.Equal(t, "sweep_expired_rotating: revoke failed", log.warnings[0].msg)
	assert.Equal(t, tenantID, log.warnings[0].fields["tenant_id"])
	assert.Equal(t, p.ID, log.warnings[0].fields["principal_id"])
	assert.Contains(t, log.warnings[0].fields["error"], "openbao unreachable")

	// The stale row must still be `rotating`, not silently marked revoked.
	stillRotating, err := credentials.FindByVersion(ctx, tenantID, p.ID, 99)
	require.NoError(t, err)
	require.NotNil(t, stillRotating)
	assert.Equal(t, domain.CredentialStatusRotating, stillRotating.Status)
}

func TestCredentialService_SweepExpiredRotating_NilLoggerIsNoop(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	events := &fakeEventPublisher{}
	generate := func() (string, error) { return uuid.NewString(), nil }
	// log is nil — matches every other test in this package's construction
	// (newTestCredentialService) — assert it doesn't panic when the sweep
	// hits a failure with no logger configured.
	svc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{events: events}, nil, generate)

	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	credentials.listByPrincipalErr = errors.New("list boom")

	require.NotPanics(t, func() {
		_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
		require.NoError(t, err)
	})
}
