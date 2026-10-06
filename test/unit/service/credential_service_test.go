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

func newTestCredentialService(t *testing.T) (*service.CredentialService, *fakePrincipalRepository, *fakeCredentialRepository, *fakeSecretStore, *fakeEventPublisher) {
	t.Helper()
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	events := &fakeEventPublisher{}
	generate := func() (string, error) {
		return uuid.NewString(), nil
	}
	svc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{events: events}, nil, generate)
	return svc, principals, credentials, secrets, events
}

func seedPrincipal(t *testing.T, repo *fakePrincipalRepository, tenantID uuid.UUID) *domain.ServiceAccountPrincipal {
	t.Helper()
	p := &domain.ServiceAccountPrincipal{
		ID: uuid.New(), TenantID: tenantID, PrincipalSub: uuid.New(),
		KeycloakClientID: domain.KeycloakClientPlatformAutomation,
		PrincipalType:    domain.PrincipalTypePlatformAutomation,
		Status:           domain.PrincipalStatusActive,
		RecordVersion:    1,
	}
	repo.put(p)
	return p
}

func TestCredentialService_IssueOrRotate_Issue(t *testing.T) {
	svc, principals, _, secrets, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	res, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, actor)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Version)
	assert.False(t, res.Replayed)
	assert.NotEmpty(t, res.Secret)
	assert.Nil(t, res.ExpiresPriorAt)
	assert.Equal(t, domain.OpenBaoPathFor(tenantID, p.KeycloakClientID, 1), res.OpenBaoPath)

	stored, err := secrets.Read(ctx, res.OpenBaoPath)
	require.NoError(t, err)
	assert.Equal(t, res.Secret, stored)

	require.Len(t, events.events, 1)
	assert.Equal(t, domain.EventServiceAccountCredentialIssued, events.events[0].Type)
}

func TestCredentialService_IssueOrRotate_Rotate(t *testing.T) {
	svc, principals, credentials, _, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	first, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, actor)
	require.NoError(t, err)

	second, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, actor)
	require.NoError(t, err)
	assert.Equal(t, 2, second.Version)
	assert.NotEqual(t, first.Secret, second.Secret)
	require.NotNil(t, second.ExpiresPriorAt)

	// Exactly one active credential remains; the first version is now rotating.
	active, err := credentials.FindActive(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, 2, active.Version)

	priorRow, err := credentials.FindByVersion(ctx, tenantID, p.ID, 1)
	require.NoError(t, err)
	require.NotNil(t, priorRow)
	assert.Equal(t, domain.CredentialStatusRotating, priorRow.Status)
	require.NotNil(t, priorRow.ExpiresAt)

	require.Len(t, events.events, 2)
	assert.Equal(t, domain.EventServiceAccountCredentialRotated, events.events[1].Type)
}

// TestCredentialService_IssueOrRotate_StampsDefaultCadence covers §16
// TSQ-6 Resolved: an issue/rotate call, from any caller, stamps the new
// `active` row's rotation_cadence_days/next_rotation_at using
// domain.DefaultCadenceDays when WithCadenceDays was never called —
// cmd/scheduler's due-list scan (idx_sac_next_rotation) depends on every
// active row carrying this, not just cadence-driven ones.
func TestCredentialService_IssueOrRotate_StampsDefaultCadence(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	before := time.Now().UTC()
	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, domain.SystemPrincipalID)
	require.NoError(t, err)

	active, err := credentials.FindActive(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.NotNil(t, active)
	require.NotNil(t, active.RotationCadenceDays)
	assert.Equal(t, domain.DefaultCadenceDays, *active.RotationCadenceDays)
	require.NotNil(t, active.NextRotationAt)
	assert.WithinDuration(t, before.AddDate(0, 0, domain.DefaultCadenceDays), *active.NextRotationAt, 5*time.Second)
}

// TestCredentialService_WithCadenceDays_OverridesDefault covers the
// ROTATION_DEFAULT_CADENCE_DAYS override path (§12) — WithCadenceDays
// changes what gets stamped without touching the constructor's signature
// or any other call site/test.
func TestCredentialService_WithCadenceDays_OverridesDefault(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	generate := func() (string, error) { return uuid.NewString(), nil }
	svc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{}, nil, generate).WithCadenceDays(30)

	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	before := time.Now().UTC()
	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, domain.SystemPrincipalID)
	require.NoError(t, err)

	active, err := credentials.FindActive(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.NotNil(t, active.RotationCadenceDays)
	assert.Equal(t, 30, *active.RotationCadenceDays)
	assert.WithinDuration(t, before.AddDate(0, 0, 30), *active.NextRotationAt, 5*time.Second)
}

// TestCredentialService_IssueOrRotate_RotateClearsPriorCadence covers §4.2:
// only the current `active` row is ever "due" — a rotate must null out the
// demoted row's cadence fields, or cmd/scheduler's due-list scan would
// keep matching a row that is no longer eligible.
func TestCredentialService_IssueOrRotate_RotateClearsPriorCadence(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, actor)
	require.NoError(t, err)

	_, err = svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, actor)
	require.NoError(t, err)

	priorRow, err := credentials.FindByVersion(ctx, tenantID, p.ID, 1)
	require.NoError(t, err)
	require.NotNil(t, priorRow)
	assert.Nil(t, priorRow.RotationCadenceDays)
	assert.Nil(t, priorRow.NextRotationAt)

	active, err := credentials.FindActive(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.NotNil(t, active.RotationCadenceDays)
	require.NotNil(t, active.NextRotationAt)
}

func TestCredentialService_IssueOrRotate_RotationIDReplayReturnsSameSecret(t *testing.T) {
	svc, principals, _, _, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID
	rotationID := uuid.New()

	first, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID, OverlapSeconds: 300}, actor)
	require.NoError(t, err)

	replay, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID, OverlapSeconds: 300}, actor)
	require.NoError(t, err)

	assert.True(t, replay.Replayed)
	assert.Equal(t, first.Version, replay.Version)
	assert.Equal(t, first.Secret, replay.Secret)
	assert.Equal(t, first.OpenBaoPath, replay.OpenBaoPath)
	// No new material generated, no new row, no new event on replay.
	assert.Len(t, events.events, 1)
}

// TestCredentialService_IssueOrRotate_RotationIDReplayAtVersionGreaterThanOne
// covers replayIssueOrRotate's "existing.Version > 1" branch — the prior
// replay test only ever replayed a version-1 issue, never a rotation, so
// the prior-version FindByVersion lookup (for ExpiresPriorAt) never ran.
func TestCredentialService_IssueOrRotate_RotationIDReplayAtVersionGreaterThanOne(t *testing.T) {
	svc, principals, _, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, actor)
	require.NoError(t, err)

	rotationID := uuid.New()
	rotated, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID, OverlapSeconds: 300}, actor)
	require.NoError(t, err)
	require.Equal(t, 2, rotated.Version)

	replay, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID, OverlapSeconds: 300}, actor)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Equal(t, 2, replay.Version)
	require.NotNil(t, replay.ExpiresPriorAt, "a version>1 replay must re-derive expires_prior_at from the prior version's row")
}

// TestCredentialService_IssueOrRotate_GenerateErrorPropagates covers
// issueOrRotate's secret-generation failure branch.
func TestCredentialService_IssueOrRotate_GenerateErrorPropagates(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	events := &fakeEventPublisher{}
	wantErr := errors.New("rand exhausted")
	generate := func() (string, error) { return "", wantErr }
	svc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{events: events}, nil, generate)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, domain.SystemPrincipalID)
	require.ErrorIs(t, err, wantErr)
}

func TestCredentialService_IssueOrRotate_MissingRotationIDIsInvalidRequest(t *testing.T) {
	svc, principals, _, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{}, domain.SystemPrincipalID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrInvalidRequest, de.Code)
}

func TestCredentialService_IssueOrRotate_RevokedPrincipalRejected(t *testing.T) {
	svc, principals, _, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	p.Status = domain.PrincipalStatusRevoked
	principals.put(p)

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalRevoked, de.Code)
}

func TestCredentialService_IssueOrRotate_UnknownPrincipalNotFound(t *testing.T) {
	svc, _, _, _, _ := newTestCredentialService(t)
	ctx := context.Background()

	_, err := svc.IssueOrRotate(ctx, uuid.New(), uuid.New(), service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalNotFound, de.Code)
}

func TestCredentialService_Revoke(t *testing.T) {
	svc, principals, credentials, secrets, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	issued, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)

	res, err := svc.Revoke(ctx, tenantID, p.ID, issued.Version, actor)
	require.NoError(t, err)
	assert.Equal(t, domain.CredentialStatusRevoked, res.Status)

	_, err = secrets.Read(ctx, issued.OpenBaoPath)
	assert.Error(t, err, "OpenBao material must be deleted on revoke")

	row, err := credentials.FindByVersion(ctx, tenantID, p.ID, issued.Version)
	require.NoError(t, err)
	assert.Equal(t, domain.CredentialStatusRevoked, row.Status)

	require.Len(t, events.events, 2) // issued + revoked
	assert.Equal(t, domain.EventServiceAccountCredentialRevoked, events.events[1].Type)
}

func TestCredentialService_Revoke_IdempotentNoOp(t *testing.T) {
	svc, principals, _, _, events := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	issued, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)

	_, err = svc.Revoke(ctx, tenantID, p.ID, issued.Version, actor)
	require.NoError(t, err)
	firstEventCount := len(events.events)

	res, err := svc.Revoke(ctx, tenantID, p.ID, issued.Version, actor)
	require.NoError(t, err)
	assert.Equal(t, domain.CredentialStatusRevoked, res.Status)
	assert.Len(t, events.events, firstEventCount, "re-revoke must not emit a second event")
}

func TestCredentialService_Revoke_UnknownVersionNotFound(t *testing.T) {
	svc, principals, _, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	_, err := svc.Revoke(ctx, tenantID, p.ID, 99, domain.SystemPrincipalID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalNotFound, de.Code)
}

// ── Repository/secret-store failure branches ────────────────────────────

func TestNewCredentialService_NilGenerateUsesDefault(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	svc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{}, nil, nil)

	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	res, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.NotEmpty(t, res.Secret, "DefaultKeyGenerator must have produced real material")
}

func TestCredentialService_IssueOrRotate_FindByRotationIDError(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	credentials.forceFindByRotationIDErr = errors.New("db unavailable")

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.EqualError(t, err, "db unavailable")
}

func TestCredentialService_IssueOrRotate_FindActiveError(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	credentials.forceFindActiveErr = errors.New("db unavailable")

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.EqualError(t, err, "db unavailable")
}

func TestCredentialService_IssueOrRotate_SecretWriteError(t *testing.T) {
	svc, principals, credentials, secrets, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	secrets.writeErr = errors.New("openbao unavailable")

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.EqualError(t, err, "openbao unavailable")

	active, err := credentials.FindActive(ctx, tenantID, p.ID)
	require.NoError(t, err)
	assert.Nil(t, active, "nothing must be committed in Postgres when the OpenBao write fails (§9.3)")
}

func TestCredentialService_IssueOrRotate_DemoteUpdateError(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)

	credentials.forceUpdateErr = errors.New("db unavailable")
	_, err = svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.EqualError(t, err, "db unavailable")
}

func TestCredentialService_IssueOrRotate_InsertError(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	credentials.forceInsertErr = errors.New("db unavailable")

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.EqualError(t, err, "db unavailable")
}

func TestCredentialService_IssueOrRotate_NoEventPublisherOnContextStillSucceeds(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	generate := func() (string, error) { return uuid.NewString(), nil }
	// fakeTxRunner with events == nil never calls WithEventPublisher, so
	// port.EventPublisherFromContext returns !ok inside issueOrRotate —
	// the write must still succeed, just without an enqueued event.
	svc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{}, nil, generate)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	res, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Version)
}

func TestCredentialService_IssueOrRotate_ReplaySecretReadError(t *testing.T) {
	svc, principals, _, secrets, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	rotationID := uuid.New()

	issued, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)

	// Simulate the OpenBao material having vanished out from under an
	// already-committed row — the replay path's Read must surface that.
	require.NoError(t, secrets.Delete(ctx, issued.OpenBaoPath))

	_, err = svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
}

// TestCredentialService_IssueOrRotate_ReplayAgainstRevokedCredentialReturnsClearError
// covers replayIssueOrRotate's explicit revoked-status branch: replaying a
// rotation_id whose row has since been revoked (a later TS-2, the overlap
// sweep, or offboarding) must return a classified 409
// credential_replay_revoked, not let the OpenBao Read fail and surface as
// a misleading 502 secret_store_unavailable (that would look like an
// OpenBao outage rather than the expected, classifiable outcome of a stale
// idempotency key).
func TestCredentialService_IssueOrRotate_ReplayAgainstRevokedCredentialReturnsClearError(t *testing.T) {
	svc, principals, _, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID
	rotationID := uuid.New()

	issued, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID, OverlapSeconds: 300}, actor)
	require.NoError(t, err)

	_, err = svc.Revoke(ctx, tenantID, p.ID, issued.Version, actor)
	require.NoError(t, err)

	_, err = svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID, OverlapSeconds: 300}, actor)
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrCredentialReplayRevoked, de.Code)
	assert.Equal(t, 409, de.Status())
}

func TestCredentialService_IssueOrRotate_ReplayPriorVersionLookupFailureLeavesExpiresPriorNil(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	// Get to version 2 (so replay's "existing.Version > 1" branch is live),
	// then replay the SECOND rotation's rotation_id.
	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)
	secondRotationID := uuid.New()
	second, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: secondRotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)
	require.Equal(t, 2, second.Version)

	// Force the prior-version lookup (version-1) to fail on replay — the
	// replay must still succeed, just with ExpiresPriorAt left nil (the
	// prior-lookup is best-effort enrichment, not required for the replay
	// itself).
	credentials.forceFindByVersionErr = errors.New("db unavailable")
	replay, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: secondRotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Nil(t, replay.ExpiresPriorAt)
}

func TestCredentialService_IssueOrRotate_ReplayPriorVersionRowMissingLeavesExpiresPriorNil(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)
	secondRotationID := uuid.New()
	second, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: secondRotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)
	require.Equal(t, 2, second.Version)

	// Simulate a data inconsistency: the prior (version 1) row is simply
	// gone (FindByVersion legitimately returns nil, nil — not an error).
	// The replay's best-effort enrichment must tolerate this, not panic or
	// fail the whole replay.
	for id, c := range credentials.byID {
		if c.TenantID == tenantID && c.PrincipalID == p.ID && c.Version == 1 {
			delete(credentials.byID, id)
		}
	}

	replay, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: secondRotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.True(t, replay.Replayed)
	assert.Nil(t, replay.ExpiresPriorAt)
}

func TestCredentialService_Revoke_FindByVersionError(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	credentials.forceFindByVersionErr = errors.New("db unavailable")

	_, err := svc.Revoke(ctx, tenantID, p.ID, 1, domain.SystemPrincipalID)
	require.EqualError(t, err, "db unavailable")
}

func TestCredentialService_Revoke_UpdateError(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	issued, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)

	credentials.forceUpdateErr = errors.New("db unavailable")
	_, err = svc.Revoke(ctx, tenantID, p.ID, issued.Version, domain.SystemPrincipalID)
	require.EqualError(t, err, "db unavailable")
}

// TestCredentialService_Revoke_ConcurrentRevokeRaceReturnsIdempotentSuccess
// covers revokeCredential's post-conflict re-check: a concurrent actor (a
// racing TS-2 call, or cmd/rotator's overlap-expiry sweep) revoking the
// exact same row between this call's FindByVersion read and its own Update
// must not surface as 409 optimistic_lock_conflict — TS-2 is documented as
// idempotent (§5.4), and that must hold against a genuine race, not only a
// stale read taken before one started.
func TestCredentialService_Revoke_ConcurrentRevokeRaceReturnsIdempotentSuccess(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	issued, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)

	var raceRevokedAt time.Time
	credentials.raceOnUpdate = func(byID map[uuid.UUID]*domain.Credential) {
		for _, c := range byID {
			if c.TenantID == tenantID && c.PrincipalID == p.ID && c.Version == issued.Version {
				raceRevokedAt = time.Now().UTC()
				c.Status = domain.CredentialStatusRevoked
				c.RevokedAt = &raceRevokedAt
				c.RecordVersion++ // the racer's own successful Update already bumped this
			}
		}
	}

	res, err := svc.Revoke(ctx, tenantID, p.ID, issued.Version, actor)
	require.NoError(t, err, "a benign concurrent-revoke race must not surface as a conflict")
	assert.Equal(t, domain.CredentialStatusRevoked, res.Status)
	assert.Equal(t, raceRevokedAt, res.RevokedAt, "must report the racer's outcome, not fabricate a new one")
}

func TestCredentialService_Revoke_NoEventPublisherOnContextStillSucceeds(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	generate := func() (string, error) { return uuid.NewString(), nil }
	events := &fakeEventPublisher{}
	// The issue itself needs an event publisher wired so we can assert
	// revoke specifically adds no second event when EventPublisherFromContext
	// is unavailable on ITS OWN call — but issueOrRotate/revoke share the
	// same TxRunner. Use a events-enabled runner for issue, then swap the
	// service to a no-events runner for the revoke call by constructing a
	// second service instance sharing the same repositories/store.
	txWithEvents := &fakeTxRunner{events: events}
	issueSvc := service.NewCredentialService(principals, credentials, secrets, txWithEvents, nil, generate)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	issued, err := issueSvc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)
	require.Len(t, events.events, 1)

	revokeSvc := service.NewCredentialService(principals, credentials, secrets, &fakeTxRunner{}, nil, generate)
	res, err := revokeSvc.Revoke(ctx, tenantID, p.ID, issued.Version, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, domain.CredentialStatusRevoked, res.Status)
	assert.Len(t, events.events, 1, "no event publisher on context means no event enqueued, not a failure")
}

// Regression (production-readiness review): revoking the ACTIVE version left
// no active row, so the next issue restarted at v1 and collided with the
// existing rows (uq_sac_version) on every retry — an unrecoverable outage on
// the documented hard-cutover path (revoke, then rotate).
func TestCredentialService_IssueAfterRevokingActiveUsesMaxPlusOne(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)
	_, err = svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)
	_, err = svc.Revoke(ctx, tenantID, p.ID, 2, actor)
	require.NoError(t, err)

	active, err := credentials.FindActive(ctx, tenantID, p.ID)
	require.NoError(t, err)
	require.Nil(t, active, "precondition: no active credential after revoking it")

	res, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Version)
}

func TestCredentialService_ExpectActiveVersionMismatchWritesNothing(t *testing.T) {
	svc, principals, credentials, secrets, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)
	_, err = svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err) // an operator rotation committed since the scheduler enumerated v1

	_, err = svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), ExpectActiveVersion: 1}, actor)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrOptimisticLockConflict, de.Code)

	maxVersion, err := credentials.MaxVersion(ctx, tenantID, p.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, maxVersion, "nothing rotated")
	_, err = secrets.Read(ctx, domain.OpenBaoPathFor(tenantID, p.KeycloakClientID, 3))
	assert.Error(t, err, "no material written for a refused rotation")
}

// TS-INV-3: at most one additional rotating version. A second rotate inside
// the overlap window ends the earlier overlap, so at most two keys stay live.
func TestCredentialService_SecondRotateWithinOverlapEndsEarlierOverlap(t *testing.T) {
	svc, principals, credentials, _, _ := newTestCredentialService(t)
	ctx := context.Background()
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	actor := domain.SystemPrincipalID

	for range 3 {
		_, err := svc.IssueOrRotate(ctx, tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 900}, actor)
		require.NoError(t, err)
	}

	creds, err := credentials.ListByPrincipal(ctx, tenantID, p.ID)
	require.NoError(t, err)
	now := time.Now().UTC()
	live := 0
	for _, c := range creds {
		if c.Status == domain.CredentialStatusActive || (c.Status == domain.CredentialStatusRotating && !c.IsExpiredOverlap(now)) {
			live++
		}
	}
	assert.Equal(t, 2, live, "the new active plus at most one open overlap")
	v1, err := credentials.FindByVersion(ctx, tenantID, p.ID, 1)
	require.NoError(t, err)
	assert.NotEqual(t, domain.CredentialStatusActive, v1.Status)
}
