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

func newTestPrincipalService(t *testing.T) (*service.PrincipalService, *fakePrincipalRepository, *fakeCredentialRepository, *fakeEventPublisher) {
	t.Helper()
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	events := &fakeEventPublisher{}
	svc := service.NewPrincipalService(principals, credentials, &fakeTxRunner{events: events})
	return svc, principals, credentials, events
}

func TestPrincipalService_Register(t *testing.T) {
	svc, _, _, events := newTestPrincipalService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	req := service.RegisterRequest{PrincipalSub: uuid.New(), KeycloakClientID: domain.KeycloakClientPlatformAutomation}

	res, err := svc.Register(ctx, tenantID, req, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.True(t, res.Created)
	assert.Equal(t, domain.PrincipalStatusActive, res.Status)
	require.Len(t, events.events, 1)
	assert.Equal(t, domain.EventServiceAccountRegistered, events.events[0].Type)
}

func TestPrincipalService_Register_IdempotentRepeat(t *testing.T) {
	svc, _, _, events := newTestPrincipalService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	req := service.RegisterRequest{PrincipalSub: uuid.New(), KeycloakClientID: domain.KeycloakClientPlatformAutomation}

	first, err := svc.Register(ctx, tenantID, req, domain.SystemPrincipalID)
	require.NoError(t, err)

	// An identical repeat (same principal_sub/keycloak_client_id) is a
	// true no-op — no second event.
	second, err := svc.Register(ctx, tenantID, req, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.False(t, second.Created)
	assert.Equal(t, first.PrincipalID, second.PrincipalID)
	assert.Len(t, events.events, 1, "an identical repeat register must not emit a second event")
}

func TestPrincipalService_Register_CarryOverUpdatesAndEmits(t *testing.T) {
	svc, _, _, events := newTestPrincipalService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	req := service.RegisterRequest{PrincipalSub: uuid.New(), KeycloakClientID: domain.KeycloakClientPlatformAutomation + "-" + tenantID.String()}

	first, err := svc.Register(ctx, tenantID, req, domain.SystemPrincipalID)
	require.NoError(t, err)

	// RP-3 conversion: a new dedicated-realm Keycloak client replaces the
	// trial-realm one — same tenant/principal row, a different sub/clientID.
	carryOver := service.RegisterRequest{PrincipalSub: uuid.New(), KeycloakClientID: domain.KeycloakClientPlatformAutomation}
	second, err := svc.Register(ctx, tenantID, carryOver, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.False(t, second.Created, "a carry-over must not report as a fresh 201 create")
	assert.Equal(t, first.PrincipalID, second.PrincipalID, "carry-over updates the same principal row")
	require.Len(t, events.events, 2, "a carry-over update must re-emit for audit continuity")
	assert.Equal(t, domain.EventServiceAccountRegistered, events.events[1].Type)
}

func TestPrincipalService_Register_InvalidKeycloakClientID(t *testing.T) {
	svc, _, _, _ := newTestPrincipalService(t)
	ctx := context.Background()

	_, err := svc.Register(ctx, uuid.New(), service.RegisterRequest{PrincipalSub: uuid.New(), KeycloakClientID: "not-platform-automation"}, domain.SystemPrincipalID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
}

func TestPrincipalService_Register_MissingPrincipalSub(t *testing.T) {
	svc, _, _, _ := newTestPrincipalService(t)
	ctx := context.Background()

	_, err := svc.Register(ctx, uuid.New(), service.RegisterRequest{KeycloakClientID: domain.KeycloakClientPlatformAutomation}, domain.SystemPrincipalID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
}

func TestPrincipalService_ReadPrincipal(t *testing.T) {
	svc, principals, credentials, _ := newTestPrincipalService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	require.NoError(t, credentials.Insert(ctx, &domain.Credential{
		TenantID: tenantID, PrincipalID: p.ID, Version: 1, Status: domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, p.KeycloakClientID, 1), GrantedBy: domain.SystemPrincipalID,
	}))

	res, err := svc.ReadPrincipal(ctx, tenantID, p.ID)
	require.NoError(t, err)
	assert.Equal(t, p.ID, res.PrincipalID)
	require.Len(t, res.Credentials, 1)
	assert.Equal(t, 1, res.Credentials[0].Version)
	assert.Equal(t, domain.CredentialStatusActive, res.Credentials[0].Status)
}

func TestPrincipalService_ReadPrincipal_NotFound(t *testing.T) {
	svc, _, _, _ := newTestPrincipalService(t)
	ctx := context.Background()

	_, err := svc.ReadPrincipal(ctx, uuid.New(), uuid.New())
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalNotFound, de.Code)
}

func TestPrincipalService_ReadPrincipal_ListByPrincipalError(t *testing.T) {
	svc, principals, credentials, _ := newTestPrincipalService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	credentials.listByPrincipalErr = errors.New("db unavailable")

	_, err := svc.ReadPrincipal(ctx, tenantID, p.ID)
	require.EqualError(t, err, "db unavailable")
}

func TestPrincipalService_Register_RepositoryError(t *testing.T) {
	svc, principals, _, _ := newTestPrincipalService(t)
	principals.forceRegisterErr = errors.New("db unavailable")

	_, err := svc.Register(context.Background(), uuid.New(),
		service.RegisterRequest{PrincipalSub: uuid.New(), KeycloakClientID: domain.KeycloakClientPlatformAutomation},
		domain.SystemPrincipalID)
	require.EqualError(t, err, "db unavailable")
}

func TestPrincipalService_Register_NoEventPublisherOnContextStillSucceeds(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	// fakeTxRunner with events == nil never calls WithEventPublisher, so
	// port.EventPublisherFromContext returns !ok inside Register — the
	// write must still succeed, just without an enqueued event.
	svc := service.NewPrincipalService(principals, credentials, &fakeTxRunner{})
	ctx := context.Background()
	req := service.RegisterRequest{PrincipalSub: uuid.New(), KeycloakClientID: domain.KeycloakClientPlatformAutomation}

	res, err := svc.Register(ctx, uuid.New(), req, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.True(t, res.Created)
}
