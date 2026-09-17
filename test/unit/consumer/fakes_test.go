package consumer_test

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// ─────────────────────────────────────────────────────────────────────────
// In-memory port fakes for the offboarding consumer's unit tests. Smaller
// and scoped differently than test/unit/service's fakes (this consumer
// needs ListByTenant + ProcessedEventsStore, not Insert/Update/rotation
// semantics), so kept separate rather than shared.
// ─────────────────────────────────────────────────────────────────────────

type fakeTxRunner struct {
	events port.EventPublisher
}

func (f *fakeTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if f.events != nil {
		ctx = port.WithEventPublisher(ctx, f.events)
	}
	return fn(ctx)
}

type fakeEventPublisher struct {
	events []*domain.Event

	// enqueueErr, when set, is returned by Enqueue instead of succeeding.
	enqueueErr error
}

func (f *fakeEventPublisher) Enqueue(_ context.Context, e *domain.Event) error {
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	f.events = append(f.events, e)
	return nil
}

type fakePrincipalRepository struct {
	byTenant map[uuid.UUID][]*domain.ServiceAccountPrincipal
	deleted  []uuid.UUID

	// listByTenantErr / deleteByTenantErr, when set, are returned by their
	// respective methods instead of the normal behavior — for exercising a
	// caller's repository-error branch.
	listByTenantErr   error
	deleteByTenantErr error
}

func newFakePrincipalRepository() *fakePrincipalRepository {
	return &fakePrincipalRepository{byTenant: map[uuid.UUID][]*domain.ServiceAccountPrincipal{}}
}

func (f *fakePrincipalRepository) seed(tenantID uuid.UUID, p *domain.ServiceAccountPrincipal) {
	f.byTenant[tenantID] = append(f.byTenant[tenantID], p)
}

func (f *fakePrincipalRepository) FindByID(_ context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	for _, p := range f.byTenant[tenantID] {
		if p.ID == principalID {
			return p, nil
		}
	}
	return nil, domain.NewError(domain.ErrPrincipalNotFound, "no principal for this tenant")
}

func (f *fakePrincipalRepository) Register(_ context.Context, p *domain.ServiceAccountPrincipal) (*domain.ServiceAccountPrincipal, bool, bool, error) {
	f.seed(p.TenantID, p)
	return p, true, false, nil
}

func (f *fakePrincipalRepository) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]*domain.ServiceAccountPrincipal, error) {
	if f.listByTenantErr != nil {
		return nil, f.listByTenantErr
	}
	return f.byTenant[tenantID], nil
}

func (f *fakePrincipalRepository) DeleteByTenant(_ context.Context, tenantID uuid.UUID) error {
	if f.deleteByTenantErr != nil {
		return f.deleteByTenantErr
	}
	f.deleted = append(f.deleted, tenantID)
	delete(f.byTenant, tenantID)
	return nil
}

var _ port.PrincipalRepository = (*fakePrincipalRepository)(nil)

type fakeCredentialRepository struct {
	byPrincipal map[uuid.UUID][]*domain.Credential

	// listByPrincipalErr, when set, is returned by ListByPrincipal instead
	// of the normal listing.
	listByPrincipalErr error
}

func newFakeCredentialRepository() *fakeCredentialRepository {
	return &fakeCredentialRepository{byPrincipal: map[uuid.UUID][]*domain.Credential{}}
}

func (f *fakeCredentialRepository) seed(principalID uuid.UUID, c *domain.Credential) {
	f.byPrincipal[principalID] = append(f.byPrincipal[principalID], c)
}

func (f *fakeCredentialRepository) FindByVersion(_ context.Context, _, principalID uuid.UUID, version int) (*domain.Credential, error) {
	for _, c := range f.byPrincipal[principalID] {
		if c.Version == version {
			return c, nil
		}
	}
	return nil, nil
}

func (f *fakeCredentialRepository) FindActive(_ context.Context, _, principalID uuid.UUID) (*domain.Credential, error) {
	for _, c := range f.byPrincipal[principalID] {
		if c.Status == domain.CredentialStatusActive {
			return c, nil
		}
	}
	return nil, nil
}

func (f *fakeCredentialRepository) FindByRotationID(_ context.Context, _, _, _ uuid.UUID) (*domain.Credential, error) {
	return nil, nil
}

func (f *fakeCredentialRepository) ListByPrincipal(_ context.Context, _, principalID uuid.UUID) ([]*domain.Credential, error) {
	if f.listByPrincipalErr != nil {
		return nil, f.listByPrincipalErr
	}
	return f.byPrincipal[principalID], nil
}

func (f *fakeCredentialRepository) Insert(_ context.Context, c *domain.Credential) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	f.seed(c.PrincipalID, c)
	return nil
}

func (f *fakeCredentialRepository) Update(_ context.Context, c *domain.Credential) error {
	for _, existing := range f.byPrincipal[c.PrincipalID] {
		if existing.ID == c.ID {
			*existing = *c
			return nil
		}
	}
	return nil
}

var _ port.CredentialRepository = (*fakeCredentialRepository)(nil)

type fakeSecretStore struct {
	deleted []string

	// deleteErr, when set, is returned by Delete instead of succeeding.
	deleteErr error
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{}
}

func (f *fakeSecretStore) Write(_ context.Context, _ string, _ string) error { return nil }
func (f *fakeSecretStore) Read(_ context.Context, _ string) (string, error)  { return "", nil }
func (f *fakeSecretStore) Delete(_ context.Context, path string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, path)
	return nil
}
func (f *fakeSecretStore) List(_ context.Context, _ string) ([]string, error) { return nil, nil }

var _ port.SecretStore = (*fakeSecretStore)(nil)

type fakeProcessedEventsStore struct {
	marked map[string]bool

	// markProcessedErr, when set, is returned by MarkProcessed instead of
	// succeeding.
	markProcessedErr error

	// isProcessedErr, when set, is returned by IsProcessed instead of
	// succeeding.
	isProcessedErr error
}

func newFakeProcessedEventsStore() *fakeProcessedEventsStore {
	return &fakeProcessedEventsStore{marked: map[string]bool{}}
}

func (f *fakeProcessedEventsStore) IsProcessed(_ context.Context, consumer port.ProcessedEventsConsumer, eventID string) (bool, error) {
	if f.isProcessedErr != nil {
		return false, f.isProcessedErr
	}
	return f.marked[string(consumer)+"/"+eventID], nil
}

func (f *fakeProcessedEventsStore) MarkProcessed(_ context.Context, consumer port.ProcessedEventsConsumer, eventID string) error {
	if f.markProcessedErr != nil {
		return f.markProcessedErr
	}
	f.marked[string(consumer)+"/"+eventID] = true
	return nil
}

func (f *fakeProcessedEventsStore) Prune(_ context.Context, _, _ int) (int, error) { return 0, nil }

var _ port.ProcessedEventsStore = (*fakeProcessedEventsStore)(nil)

// fakeLogger records every Debug call for assertion — used to prove
// OffboardingConsumer.debug's real logging branch fires (c.log non-nil),
// not just the nil-log no-op.
type fakeLogger struct {
	debugCalls  []string
	debugFields []map[string]any
	infoCalls   []string
	errorCalls  []string
	errorFields []map[string]any
}

func (f *fakeLogger) Debug(msg string, fields map[string]any) {
	f.debugCalls = append(f.debugCalls, msg)
	f.debugFields = append(f.debugFields, fields)
}
func (f *fakeLogger) Info(msg string, _ map[string]any) { f.infoCalls = append(f.infoCalls, msg) }
func (f *fakeLogger) Warn(string, map[string]any)       {}
func (f *fakeLogger) Error(msg string, fields map[string]any) {
	f.errorCalls = append(f.errorCalls, msg)
	f.errorFields = append(f.errorFields, fields)
}

var _ port.Logger = (*fakeLogger)(nil)
