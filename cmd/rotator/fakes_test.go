package main

// In-memory port fakes for cmd/rotator's job unit tests. These live in
// package main (not under test/, TS's usual convention) because Go
// forbids importing package main from any other package — there is no way
// to unit-test this component's private job functions from an external
// test package.

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

type fakeReconcilerRepository struct {
	expired []port.ExpiredRotatingCredential
	states  []port.PrincipalMaterialState
	err     error
	notLive map[uuid.UUID]bool // principals whose credentials read as revoked on re-check
}

func (f *fakeReconcilerRepository) ListExpiredRotating(context.Context, int) ([]port.ExpiredRotatingCredential, error) {
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

// IsCredentialLive reports liveness from the seeded states (a test can
// override it with notLive to simulate a revoke after the snapshot).
func (f *fakeReconcilerRepository) IsCredentialLive(_ context.Context, principalID uuid.UUID, version int) (bool, error) {
	if f.notLive[principalID] {
		return false, nil
	}
	for _, st := range f.states {
		if st.PrincipalID != principalID {
			continue
		}
		for _, c := range st.Credentials {
			if c.Version == version {
				return c.Live, nil
			}
		}
	}
	return false, nil
}

// ListDueForRotation is unused by cmd/rotator (§16 TSQ-6 Resolved,
// TS-D14 — that's cmd/scheduler's job); stubbed only to satisfy
// port.ReconcilerRepository.
func (f *fakeReconcilerRepository) ListDueForRotation(context.Context, int) ([]port.DueForRotation, error) {
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

	// realistic makes the fake behave like the Postgres repository: reads
	// return copies, and Update enforces record_version (optimistic lock)
	// and scans the bumped value back into its argument.
	realistic bool
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
			if f.realistic {
				cp := *c
				return &cp, nil
			}
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

func (f *fakeCredentialRepository) MaxVersion(_ context.Context, tenantID, principalID uuid.UUID) (int, error) {
	maxVersion := 0
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Version > maxVersion {
			maxVersion = c.Version
		}
	}
	return maxVersion, nil
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
	if existing, ok := f.byID[c.ID]; ok && f.realistic {
		if existing.RecordVersion != c.RecordVersion {
			return domain.NewError(domain.ErrOptimisticLockConflict, "record_version conflict")
		}
		c.RecordVersion++
		cp := *c
		f.byID[c.ID] = &cp
		return nil
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

	// afterCommit, when set, runs after every successful closure — a test
	// uses it to cancel the run context right after a revoke commits.
	afterCommit func()
	commits     int
}

func (f *fakeTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if f.events != nil {
		ctx = port.WithEventPublisher(ctx, f.events)
	}
	if err := fn(ctx); err != nil {
		return err
	}
	f.commits++
	if f.afterCommit != nil {
		f.afterCommit()
	}
	return nil
}

// rollbackOnceTxRunner simulates pgcommon's retry of a closure after a
// serialization failure: the first attempt runs, its writes to the
// credential fake are rolled back, and the closure runs again.
type rollbackOnceTxRunner struct {
	credentials *fakeCredentialRepository
	attempts    int
}

func (r *rollbackOnceTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	snapshot := map[uuid.UUID]domain.Credential{}
	for id, c := range r.credentials.byID {
		snapshot[id] = *c
	}
	r.attempts++
	if err := fn(ctx); err != nil {
		return err
	}
	for id, c := range snapshot {
		c := c
		r.credentials.byID[id] = &c
	}
	r.attempts++
	return fn(ctx)
}

type fakeEventPublisher struct {
	events []*domain.Event
}

func (f *fakeEventPublisher) Enqueue(_ context.Context, e *domain.Event) error {
	f.events = append(f.events, e)
	return nil
}

type fakeInboxPruner struct {
	pruneN    int64
	pruneErr  error
	consumer  port.ProcessedEventsConsumer
	retention time.Duration
	batch     int
}

func (f *fakeInboxPruner) Prune(_ context.Context, consumer port.ProcessedEventsConsumer, retention time.Duration, batch int) (int64, error) {
	f.consumer, f.retention, f.batch = consumer, retention, batch
	return f.pruneN, f.pruneErr
}

var _ port.InboxPruner = (*fakeInboxPruner)(nil)

// fakeRealmProvisionerClient stubs port.RealmProvisionerClient — an
// optional per-tenant error lets a test fail exactly one RP-17 key-refresh
// without affecting the rest of the sweep.
type fakeRealmProvisionerClient struct {
	errs      map[uuid.UUID]error
	refreshed []uuid.UUID // tenantIDs RefreshKeys was called for, in order
	ctxErrs   []error     // ctx.Err() observed at each call — must be nil even after the run ctx is cancelled
	onRefresh func(tenantID uuid.UUID)
}

func newFakeRealmProvisionerClient() *fakeRealmProvisionerClient {
	return &fakeRealmProvisionerClient{errs: map[uuid.UUID]error{}}
}

func (f *fakeRealmProvisionerClient) RefreshKeys(ctx context.Context, tenantID uuid.UUID) error {
	f.refreshed = append(f.refreshed, tenantID)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.onRefresh != nil {
		f.onRefresh(tenantID)
	}
	if err, ok := f.errs[tenantID]; ok {
		return err
	}
	return nil
}

// fakeKeysRefreshMarkers is an in-memory keys_refresh_pending. Marker
// values come from a monotonic fake clock so a test can reason about
// Clear's upTo guard exactly.
type fakeKeysRefreshMarkers struct {
	mu      sync.Mutex
	clock   time.Time
	pending map[uuid.UUID]time.Time
	hasPrin map[uuid.UUID]bool // ListPending's HasPrincipal; default true
	marks   int
	clears  []uuid.UUID
	markErr error
	listErr error
}

func newFakeKeysRefreshMarkers() *fakeKeysRefreshMarkers {
	return &fakeKeysRefreshMarkers{clock: time.Unix(1_700_000_000, 0), pending: map[uuid.UUID]time.Time{}, hasPrin: map[uuid.UUID]bool{}}
}

func (f *fakeKeysRefreshMarkers) MarkPending(_ context.Context, tenantID uuid.UUID) (pgadapter.KeysRefreshMark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return pgadapter.KeysRefreshMark{}, f.markErr
	}
	f.marks++
	f.clock = f.clock.Add(time.Second)
	_, existed := f.pending[tenantID]
	f.pending[tenantID] = f.clock
	return pgadapter.KeysRefreshMark{RequestedAt: f.clock, Fresh: !existed}, nil
}

func (f *fakeKeysRefreshMarkers) Clear(_ context.Context, tenantID uuid.UUID, upTo time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears = append(f.clears, tenantID)
	if at, ok := f.pending[tenantID]; ok && !at.After(upTo) {
		delete(f.pending, tenantID)
	}
	return nil
}

func (f *fakeKeysRefreshMarkers) ListPending(context.Context, int) ([]pgadapter.PendingKeysRefresh, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []pgadapter.PendingKeysRefresh
	for id, at := range f.pending {
		has, set := f.hasPrin[id]
		out = append(out, pgadapter.PendingKeysRefresh{TenantID: id, RequestedAt: at, HasPrincipal: !set || has})
	}
	return out, nil
}

func (f *fakeKeysRefreshMarkers) isPending(tenantID uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.pending[tenantID]
	return ok
}

var _ keysRefreshMarkers = (*fakeKeysRefreshMarkers)(nil)

// fakePrincipalLocker stubs principalLocker: a per-principal error (e.g.
// domain.ErrRotationInFlight for a 55P03 lock timeout) and a hook that runs
// while the "lock" is held, so a test can simulate a TS-1 committing the
// version just before the reconciler acquired the lock.
type fakePrincipalLocker struct {
	errs     map[uuid.UUID]error
	onLock   func(principalID uuid.UUID)
	lockedBy []uuid.UUID
}

func (f *fakePrincipalLocker) LockForUpdate(_ context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	f.lockedBy = append(f.lockedBy, principalID)
	if err := f.errs[principalID]; err != nil {
		return nil, err
	}
	if f.onLock != nil {
		f.onLock(principalID)
	}
	return &domain.ServiceAccountPrincipal{ID: principalID, TenantID: tenantID}, nil
}

var _ principalLocker = (*fakePrincipalLocker)(nil)

var _ port.RealmProvisionerClient = (*fakeRealmProvisionerClient)(nil)
