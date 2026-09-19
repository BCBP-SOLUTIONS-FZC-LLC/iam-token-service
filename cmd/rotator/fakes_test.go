package main

// In-memory port fakes for cmd/rotator's job unit tests. These live in
// package main (not under test/, TS's usual convention) because Go
// forbids importing package main from any other package — there is no way
// to unit-test this component's private job functions from an external
// test package.

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

type fakeReconcilerRepository struct {
	expired []port.ExpiredRotatingCredential
	states  []port.PrincipalMaterialState
	err     error
}

func (f *fakeReconcilerRepository) ListExpiredRotating(context.Context) ([]port.ExpiredRotatingCredential, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.expired, nil
}

func (f *fakeReconcilerRepository) ListPrincipalMaterialStates(context.Context) ([]port.PrincipalMaterialState, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.states, nil
}

// ListDueForRotation is unused by cmd/rotator (§16 TSQ-6 Resolved,
// TS-D14 — that's cmd/scheduler's job); stubbed only to satisfy
// port.ReconcilerRepository.
func (f *fakeReconcilerRepository) ListDueForRotation(context.Context) ([]port.DueForRotation, error) {
	return nil, nil
}

var _ port.ReconcilerRepository = (*fakeReconcilerRepository)(nil)

type fakeCredentialRepository struct {
	byID map[uuid.UUID]*domain.Credential

	// forceUpdateErr, when set, is returned by Update instead of applying
	// the write — used to simulate a concurrent actor (another rotator run,
	// or a racing TS-2 call) winning the optimistic-lock race on this exact
	// row between this test's FindByVersion read and its own Update.
	forceUpdateErr error
}

func newFakeCredentialRepository() *fakeCredentialRepository {
	return &fakeCredentialRepository{byID: map[uuid.UUID]*domain.Credential{}}
}

func (f *fakeCredentialRepository) seed(c *domain.Credential) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	f.byID[c.ID] = c
}

func (f *fakeCredentialRepository) FindByVersion(_ context.Context, tenantID, principalID uuid.UUID, version int) (*domain.Credential, error) {
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Version == version {
			return c, nil
		}
	}
	return nil, nil
}

func (f *fakeCredentialRepository) FindActive(_ context.Context, tenantID, principalID uuid.UUID) (*domain.Credential, error) {
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Status == domain.CredentialStatusActive {
			return c, nil
		}
	}
	return nil, nil
}

func (f *fakeCredentialRepository) FindByRotationID(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*domain.Credential, error) {
	return nil, nil
}

func (f *fakeCredentialRepository) ListByPrincipal(_ context.Context, tenantID, principalID uuid.UUID) ([]*domain.Credential, error) {
	var out []*domain.Credential
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeCredentialRepository) Insert(_ context.Context, c *domain.Credential) error {
	f.seed(c)
	return nil
}

func (f *fakeCredentialRepository) Update(_ context.Context, c *domain.Credential) error {
	if f.forceUpdateErr != nil {
		return f.forceUpdateErr
	}
	if existing, ok := f.byID[c.ID]; ok {
		*existing = *c
		return nil
	}
	f.seed(c)
	return nil
}

var _ port.CredentialRepository = (*fakeCredentialRepository)(nil)

type fakeSecretStore struct {
	data    map[string]string
	deleted []string
	listErr error

	// delErrPaths makes Delete fail only for the listed paths, so a test
	// can exercise "one row fails, the rest still succeed" without every
	// Delete call in the run failing.
	delErrPaths map[string]error

	// byPrefix lets tests seed exactly what List returns for a given
	// prefix, independent of Write/data — the real OpenBao KV List
	// returns leaf key segments like "v3", not full paths.
	byPrefix map[string][]string
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{data: map[string]string{}, byPrefix: map[string][]string{}, delErrPaths: map[string]error{}}
}

func (f *fakeSecretStore) Write(_ context.Context, path, secret string) error {
	f.data[path] = secret
	return nil
}

func (f *fakeSecretStore) Read(_ context.Context, path string) (string, error) {
	return f.data[path], nil
}

func (f *fakeSecretStore) Delete(_ context.Context, path string) error {
	if err := f.delErrPaths[path]; err != nil {
		return err
	}
	delete(f.data, path)
	f.deleted = append(f.deleted, path)
	return nil
}

func (f *fakeSecretStore) List(_ context.Context, pathPrefix string) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byPrefix[pathPrefix], nil
}

var _ port.SecretStore = (*fakeSecretStore)(nil)

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
}

func (f *fakeEventPublisher) Enqueue(_ context.Context, e *domain.Event) error {
	f.events = append(f.events, e)
	return nil
}

type fakeProcessedEventsStore struct {
	pruneN   int
	pruneErr error
}

func (f *fakeProcessedEventsStore) IsProcessed(context.Context, port.ProcessedEventsConsumer, string) (bool, error) {
	return false, nil
}
func (f *fakeProcessedEventsStore) MarkProcessed(context.Context, port.ProcessedEventsConsumer, string) error {
	return nil
}
func (f *fakeProcessedEventsStore) Prune(context.Context, int, int) (int, error) {
	return f.pruneN, f.pruneErr
}

var _ port.ProcessedEventsStore = (*fakeProcessedEventsStore)(nil)

// fakeRealmProvisionerClient stubs port.RealmProvisionerClient — an
// optional per-tenant error lets a test fail exactly one RP-17 key-refresh
// without affecting the rest of the sweep.
type fakeRealmProvisionerClient struct {
	errs      map[uuid.UUID]error
	refreshed []uuid.UUID // tenantIDs RefreshKeys was called for, in order
}

func newFakeRealmProvisionerClient() *fakeRealmProvisionerClient {
	return &fakeRealmProvisionerClient{errs: map[uuid.UUID]error{}}
}

func (f *fakeRealmProvisionerClient) RefreshKeys(_ context.Context, tenantID uuid.UUID) error {
	f.refreshed = append(f.refreshed, tenantID)
	if err, ok := f.errs[tenantID]; ok {
		return err
	}
	return nil
}

var _ port.RealmProvisionerClient = (*fakeRealmProvisionerClient)(nil)
