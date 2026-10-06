package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// ── minimal local port fakes ────────────────────────────────────────────
// Deliberately trimmed to only what CredentialService's issue/rotate/revoke
// paths call — this package only needs to observe the decorator's metric
// side effects, not re-verify CredentialService's own business logic
// (already covered by test/unit/service).

type fakeTxRunner struct{ events port.EventPublisher }

func (f *fakeTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if f.events != nil {
		ctx = port.WithEventPublisher(ctx, f.events)
	}
	return fn(ctx)
}

type fakeEventPublisher struct{ events []*domain.Event }

func (f *fakeEventPublisher) Enqueue(_ context.Context, e *domain.Event) error {
	f.events = append(f.events, e)
	return nil
}

type principalKey struct{ tenantID, id uuid.UUID }

type fakePrincipalRepository struct {
	byID map[principalKey]*domain.ServiceAccountPrincipal
}

func newFakePrincipalRepository() *fakePrincipalRepository {
	return &fakePrincipalRepository{byID: map[principalKey]*domain.ServiceAccountPrincipal{}}
}

func (f *fakePrincipalRepository) put(p *domain.ServiceAccountPrincipal) {
	f.byID[principalKey{p.TenantID, p.ID}] = p
}

func (f *fakePrincipalRepository) FindByID(_ context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	p, ok := f.byID[principalKey{tenantID, principalID}]
	if !ok {
		return nil, domain.NewError(domain.ErrPrincipalNotFound, "no principal for this tenant")
	}
	cp := *p
	return &cp, nil
}

// LockForUpdate behaves like FindByID in the fake (no real row locks).
func (f *fakePrincipalRepository) LockForUpdate(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	return f.FindByID(ctx, tenantID, principalID)
}

func (f *fakePrincipalRepository) Register(_ context.Context, p *domain.ServiceAccountPrincipal) (*domain.ServiceAccountPrincipal, bool, bool, error) {
	np := *p
	if np.ID == uuid.Nil {
		np.ID = uuid.New()
	}
	f.put(&np)
	cp := np
	return &cp, true, false, nil
}

func (f *fakePrincipalRepository) FindByPrincipalSub(_ context.Context, tenantID, principalSub uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	for _, p := range f.byID {
		if p.TenantID == tenantID && p.PrincipalSub == principalSub {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.NewError(domain.ErrPrincipalNotFound, "no principal for this tenant")
}

func (f *fakePrincipalRepository) FindByType(_ context.Context, tenantID uuid.UUID, principalType domain.PrincipalType) (*domain.ServiceAccountPrincipal, error) {
	for _, p := range f.byID {
		if p.TenantID == tenantID && p.PrincipalType == principalType {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.NewError(domain.ErrPrincipalNotFound, "no principal for this tenant")
}

func (f *fakePrincipalRepository) ListByTenant(context.Context, uuid.UUID) ([]*domain.ServiceAccountPrincipal, error) {
	return nil, nil
}
func (f *fakePrincipalRepository) DeleteByTenant(context.Context, uuid.UUID) error { return nil }

var _ port.PrincipalRepository = (*fakePrincipalRepository)(nil)

type fakeCredentialRepository struct {
	byID map[uuid.UUID]*domain.Credential
}

func newFakeCredentialRepository() *fakeCredentialRepository {
	return &fakeCredentialRepository{byID: map[uuid.UUID]*domain.Credential{}}
}

func (f *fakeCredentialRepository) FindByVersion(_ context.Context, tenantID, principalID uuid.UUID, version int) (*domain.Credential, error) {
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Version == version {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeCredentialRepository) FindActive(_ context.Context, tenantID, principalID uuid.UUID) (*domain.Credential, error) {
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Status == domain.CredentialStatusActive {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

// MaxVersion returns the highest version stored for the principal (any status).
func (f *fakeCredentialRepository) MaxVersion(_ context.Context, tenantID, principalID uuid.UUID) (int, error) {
	maxVersion := 0
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Version > maxVersion {
			maxVersion = c.Version
		}
	}
	return maxVersion, nil
}

func (f *fakeCredentialRepository) FindByRotationID(_ context.Context, tenantID, principalID, rotationID uuid.UUID) (*domain.Credential, error) {
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.RotationID != nil && *c.RotationID == rotationID {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeCredentialRepository) ListByPrincipal(_ context.Context, tenantID, principalID uuid.UUID) ([]*domain.Credential, error) {
	var out []*domain.Credential
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeCredentialRepository) Insert(_ context.Context, c *domain.Credential) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.RecordVersion = 1
	if c.IssuedAt.IsZero() {
		// The real column defaults to now(); the rotation_id replay window
		// is measured from it.
		c.IssuedAt = time.Now().UTC()
	}
	cp := *c
	f.byID[cp.ID] = &cp
	return nil
}

func (f *fakeCredentialRepository) Update(_ context.Context, c *domain.Credential) error {
	existing, ok := f.byID[c.ID]
	if !ok || existing.RecordVersion != c.RecordVersion {
		return domain.NewError(domain.ErrOptimisticLockConflict, "record_version conflict")
	}
	c.RecordVersion = existing.RecordVersion + 1
	cp := *c
	f.byID[cp.ID] = &cp
	return nil
}

var _ port.CredentialRepository = (*fakeCredentialRepository)(nil)

type fakeSecretStore struct{ data map[string]string }

func newFakeSecretStore() *fakeSecretStore { return &fakeSecretStore{data: map[string]string{}} }

func (f *fakeSecretStore) Write(_ context.Context, path, secret string) error {
	f.data[path] = secret
	return nil
}
func (f *fakeSecretStore) Read(_ context.Context, path string) (string, error) {
	v, ok := f.data[path]
	if !ok {
		return "", domain.NewError(domain.ErrSecretStoreUnavailable, "no secret at path")
	}
	return v, nil
}
func (f *fakeSecretStore) Delete(_ context.Context, path string) error {
	delete(f.data, path)
	return nil
}
func (f *fakeSecretStore) List(context.Context, string) ([]string, error) { return nil, nil }

var _ port.SecretStore = (*fakeSecretStore)(nil)

// newTestCredentialService builds a real *service.CredentialService wired
// to fresh in-memory fakes, with a fixed principal already registered —
// InstrumentedCredentialService.inner is a concrete *service.CredentialService
// (not an interface), so the decorator can only be exercised against a
// real instance.
func newTestCredentialService(t *testing.T) (*service.CredentialService, uuid.UUID, uuid.UUID) {
	t.Helper()
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	tx := &fakeTxRunner{events: &fakeEventPublisher{}}

	tenantID, principalID := uuid.New(), uuid.New()
	principals.put(&domain.ServiceAccountPrincipal{
		ID: principalID, TenantID: tenantID, PrincipalSub: uuid.New(),
		KeycloakClientID: domain.KeycloakClientPlatformAutomation,
		PrincipalType:    domain.PrincipalTypePlatformAutomation,
		Status:           domain.PrincipalStatusActive,
	})

	svc := service.NewCredentialService(principals, credentials, secrets, tx, nil, nil)
	return svc, tenantID, principalID
}

func TestInstrumentedCredentialService_IssueOrRotate_FirstIssueIncrementsIssueCounter(t *testing.T) {
	Register("test")
	before := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("issue"))

	inner, tenantID, principalID := newTestCredentialService(t)
	decorated := NewInstrumentedCredentialService(inner)

	res, err := decorated.IssueOrRotate(context.Background(), tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, uuid.New())
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, 1, res.Version)

	after := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("issue"))
	assert.Equal(t, before+1, after, "a first-ever issue must increment op=issue")
}

func TestInstrumentedCredentialService_IssueOrRotate_RotationIncrementsRotateCounter(t *testing.T) {
	Register("test")

	inner, tenantID, principalID := newTestCredentialService(t)
	decorated := NewInstrumentedCredentialService(inner)
	ctx := context.Background()
	actor := uuid.New()

	_, err := decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)

	before := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("rotate"))
	res, err := decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Version)

	after := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("rotate"))
	assert.Equal(t, before+1, after, "a rotation (version > 1, ExpiresPriorAt set) must increment op=rotate")
}

func TestInstrumentedCredentialService_IssueOrRotate_ReplayDoesNotIncrementCounter(t *testing.T) {
	Register("test")

	inner, tenantID, principalID := newTestCredentialService(t)
	decorated := NewInstrumentedCredentialService(inner)
	ctx := context.Background()
	actor := uuid.New()
	rotationID := uuid.New()

	first, err := decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, actor)
	require.NoError(t, err)
	assert.False(t, first.Replayed)

	before := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("issue"))
	replayed, err := decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, actor)
	require.NoError(t, err)
	assert.True(t, replayed.Replayed)

	after := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("issue"))
	assert.Equal(t, before, after, "a rotation_id replay must not increment the counter again")
}

func TestInstrumentedCredentialService_IssueOrRotate_ErrorDoesNotIncrementCounter(t *testing.T) {
	Register("test")

	inner, tenantID, _ := newTestCredentialService(t)
	decorated := NewInstrumentedCredentialService(inner)

	before := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("issue"))
	// An unknown principal ID -> FindByID fails -> error, before any
	// counter increment.
	_, err := decorated.IssueOrRotate(context.Background(), tenantID, uuid.New(), service.IssueOrRotateRequest{RotationID: uuid.New()}, uuid.New())
	require.Error(t, err)

	after := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("issue"))
	assert.Equal(t, before, after, "an error must not increment the counter")
}

func TestInstrumentedCredentialService_Revoke_SuccessIncrementsRevokeCounter(t *testing.T) {
	Register("test")

	inner, tenantID, principalID := newTestCredentialService(t)
	decorated := NewInstrumentedCredentialService(inner)
	ctx := context.Background()
	actor := uuid.New()

	issued, err := decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, actor)
	require.NoError(t, err)

	before := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("revoke"))
	res, err := decorated.Revoke(ctx, tenantID, principalID, issued.Version, actor)
	require.NoError(t, err)
	assert.Equal(t, domain.CredentialStatusRevoked, res.Status)

	after := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("revoke"))
	assert.Equal(t, before+1, after)
}

func TestInstrumentedCredentialService_Revoke_ErrorDoesNotIncrementCounter(t *testing.T) {
	Register("test")

	inner, tenantID, principalID := newTestCredentialService(t)
	decorated := NewInstrumentedCredentialService(inner)

	before := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("revoke"))
	// No credential at version 1 yet for this principal -> "not found" error.
	_, err := decorated.Revoke(context.Background(), tenantID, principalID, 1, uuid.New())
	require.Error(t, err)

	after := testutilCounterValue(t, CredentialsIssuedTotal.WithLabelValues("revoke"))
	assert.Equal(t, before, after)
}

// LockByTenant behaves like ListByTenant in the fake (no real row locks).
func (f *fakePrincipalRepository) LockByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.ServiceAccountPrincipal, error) {
	return f.ListByTenant(ctx, tenantID)
}

// TS-D23: every rotation_id replay is counted by outcome — served (the key
// was handed out again), revoked and expired (refused).
func TestInstrumentedCredentialService_IssueOrRotate_CountsReplaysByResult(t *testing.T) {
	Register("test")
	ctx := context.Background()
	actor := uuid.New()
	replays := func(result string) float64 {
		return testutilCounterValue(t, CredentialReplaysTotal.WithLabelValues(result))
	}

	t.Run("served", func(t *testing.T) {
		inner, tenantID, principalID := newTestCredentialService(t)
		decorated := NewInstrumentedCredentialService(inner)
		rotationID := uuid.New()
		_, err := decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, actor)
		require.NoError(t, err)
		before := replays(ReplayServed)
		_, err = decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, actor)
		require.NoError(t, err)
		assert.InDelta(t, before+1, replays(ReplayServed), 0)
	})

	t.Run("revoked", func(t *testing.T) {
		inner, tenantID, principalID := newTestCredentialService(t)
		decorated := NewInstrumentedCredentialService(inner)
		rotationID := uuid.New()
		issued, err := decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, actor)
		require.NoError(t, err)
		_, err = decorated.Revoke(ctx, tenantID, principalID, issued.Version, actor)
		require.NoError(t, err)
		before, beforeServed := replays(ReplayRevoked), replays(ReplayServed)
		_, err = decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, actor)
		require.Error(t, err)
		assert.InDelta(t, before+1, replays(ReplayRevoked), 0)
		assert.InDelta(t, beforeServed, replays(ReplayServed), 0)
	})

	t.Run("expired", func(t *testing.T) {
		inner, tenantID, principalID := newTestCredentialService(t)
		inner.WithReplayWindow(time.Nanosecond)
		decorated := NewInstrumentedCredentialService(inner)
		rotationID := uuid.New()
		_, err := decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, actor)
		require.NoError(t, err)
		time.Sleep(time.Millisecond) // past the 1ns window
		before := replays(ReplayExpired)
		_, err = decorated.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, actor)
		require.Error(t, err)
		assert.InDelta(t, before+1, replays(ReplayExpired), 0)
	})
}
