package service_test

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// ─────────────────────────────────────────────────────────────────────────
// In-memory port fakes for service-layer unit tests. Each fake mirrors the
// invariant the real Postgres/OpenBao adapter enforces (uq_sac_one_active,
// uq_sac_version -> rotation_in_flight, optimistic-lock on Update) closely
// enough to exercise CredentialService/PrincipalService business logic
// without a real database.
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
}

func (f *fakeEventPublisher) Enqueue(_ context.Context, e *domain.Event) error {
	f.events = append(f.events, e)
	return nil
}

type principalKey struct {
	tenantID uuid.UUID
	id       uuid.UUID
}

type fakePrincipalRepository struct {
	byID map[principalKey]*domain.ServiceAccountPrincipal

	// forceFindByIDErr, when non-nil, is returned by FindByID instead of its
	// normal lookup — for exercising a caller's repository-error branch.
	forceFindByIDErr error

	// forceRegisterErr, when non-nil, is returned by Register instead of
	// its normal idempotent-create logic.
	forceRegisterErr error

	// forceListByTenantErr, when non-nil, is returned by ListByTenant
	// instead of its normal listing — exercises JWKSService.PublicKeys'
	// repository-error branch.
	forceListByTenantErr error

	// forceLockErr, when non-nil, is returned by LockForUpdate — e.g. the
	// principal-lock wait timeout (rotation_in_flight with no details).
	forceLockErr error
}

func newFakePrincipalRepository() *fakePrincipalRepository {
	return &fakePrincipalRepository{byID: map[principalKey]*domain.ServiceAccountPrincipal{}}
}

func (f *fakePrincipalRepository) put(p *domain.ServiceAccountPrincipal) {
	f.byID[principalKey{p.TenantID, p.ID}] = p
}

func (f *fakePrincipalRepository) FindByID(_ context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	if f.forceFindByIDErr != nil {
		return nil, f.forceFindByIDErr
	}
	p, ok := f.byID[principalKey{tenantID, principalID}]
	if !ok {
		return nil, domain.NewError(domain.ErrPrincipalNotFound, "no principal for this tenant")
	}
	cp := *p
	return &cp, nil
}

// LockForUpdate behaves like FindByID in the fake (no real row locks).
func (f *fakePrincipalRepository) LockForUpdate(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	if f.forceLockErr != nil {
		return nil, f.forceLockErr
	}
	return f.FindByID(ctx, tenantID, principalID)
}

func (f *fakePrincipalRepository) Register(_ context.Context, p *domain.ServiceAccountPrincipal) (*domain.ServiceAccountPrincipal, bool, bool, error) {
	if f.forceRegisterErr != nil {
		return nil, false, false, f.forceRegisterErr
	}
	for _, existing := range f.byID {
		if existing.TenantID == p.TenantID && existing.PrincipalType == p.PrincipalType {
			// A true no-op repeat (identity unchanged) leaves the row alone;
			// a carry-over (RP-3 conversion, differing principal_sub/
			// keycloak_client_id) updates it in place — mirrors the
			// postgres adapter's ON CONFLICT ... WHERE ... DO UPDATE.
			if existing.PrincipalSub == p.PrincipalSub && existing.KeycloakClientID == p.KeycloakClientID {
				cp := *existing
				return &cp, false, false, nil
			}
			existing.PrincipalSub = p.PrincipalSub
			existing.KeycloakClientID = p.KeycloakClientID
			cp := *existing
			return &cp, false, true, nil
		}
	}
	np := *p
	if np.ID == uuid.Nil {
		np.ID = uuid.New()
	}
	if np.Status == "" {
		np.Status = domain.PrincipalStatusActive // mirrors the schema's DEFAULT 'active' (§4.2)
	}
	np.RecordVersion = 1
	f.put(&np)
	cp := np
	return &cp, true, false, nil
}

func (f *fakePrincipalRepository) FindByPrincipalSub(_ context.Context, tenantID, principalSub uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	if f.forceFindByIDErr != nil {
		return nil, f.forceFindByIDErr
	}
	for _, p := range f.byID {
		if p.TenantID == tenantID && p.PrincipalSub == principalSub {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.NewError(domain.ErrPrincipalNotFound, "no principal for this tenant")
}

func (f *fakePrincipalRepository) FindByType(_ context.Context, tenantID uuid.UUID, principalType domain.PrincipalType) (*domain.ServiceAccountPrincipal, error) {
	if f.forceFindByIDErr != nil {
		return nil, f.forceFindByIDErr
	}
	for _, p := range f.byID {
		if p.TenantID == tenantID && p.PrincipalType == principalType {
			cp := *p
			return &cp, nil
		}
	}
	return nil, domain.NewError(domain.ErrPrincipalNotFound, "no principal for this tenant")
}

func (f *fakePrincipalRepository) ListByTenant(_ context.Context, tenantID uuid.UUID) ([]*domain.ServiceAccountPrincipal, error) {
	if f.forceListByTenantErr != nil {
		return nil, f.forceListByTenantErr
	}
	var out []*domain.ServiceAccountPrincipal
	for k, p := range f.byID {
		if k.tenantID == tenantID {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakePrincipalRepository) DeleteByTenant(_ context.Context, tenantID uuid.UUID) error {
	for k := range f.byID {
		if k.tenantID == tenantID {
			delete(f.byID, k)
		}
	}
	return nil
}

var _ port.PrincipalRepository = (*fakePrincipalRepository)(nil)

type fakeCredentialRepository struct {
	byID map[uuid.UUID]*domain.Credential

	// listByPrincipalErr, when set, is returned by ListByPrincipal instead
	// of the normal listing — lets a test drive
	// CredentialService.sweepExpiredRotating's list-failure warn() branch.
	listByPrincipalErr error

	// forceFindByVersionErr / forceFindActiveErr / forceFindByRotationIDErr,
	// when set, are returned by their respective methods instead of the
	// normal lookup — for exercising a caller's repository-error branch.
	forceFindByVersionErr    error
	forceFindActiveErr       error
	forceFindByRotationIDErr error

	// forceUpdateErr, when set, is returned by Update instead of applying
	// the write — distinct from the optimistic-lock conflict Update already
	// returns on a stale record_version, for exercising a plain
	// repository-failure branch (e.g. a connectivity error mid-transaction).
	forceUpdateErr error

	// forceInsertErr, when set, is returned by Insert instead of applying
	// the write — distinct from the uq_sac_one_active/uq_sac_version
	// conflict Insert already detects, for exercising a plain
	// repository-failure branch that the single-threaded service-layer
	// flow can otherwise never trigger (Insert only naturally conflicts
	// under real concurrency).
	forceInsertErr error

	// raceOnUpdate, when set, is invoked once (then cleared) at the start
	// of Update, before the optimistic-lock check — lets a test simulate a
	// concurrent actor (another revoke call, or the overlap-expiry sweep)
	// completing first and mutating the same row, so this call's own
	// Update then naturally hits a genuine record_version mismatch via the
	// real optimistic-lock check below, rather than a fabricated error.
	raceOnUpdate func(byID map[uuid.UUID]*domain.Credential)
}

func newFakeCredentialRepository() *fakeCredentialRepository {
	return &fakeCredentialRepository{byID: map[uuid.UUID]*domain.Credential{}}
}

func (f *fakeCredentialRepository) FindByVersion(_ context.Context, tenantID, principalID uuid.UUID, version int) (*domain.Credential, error) {
	if f.forceFindByVersionErr != nil {
		return nil, f.forceFindByVersionErr
	}
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.Version == version {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeCredentialRepository) FindActive(_ context.Context, tenantID, principalID uuid.UUID) (*domain.Credential, error) {
	if f.forceFindActiveErr != nil {
		return nil, f.forceFindActiveErr
	}
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
	if f.forceFindByRotationIDErr != nil {
		return nil, f.forceFindByRotationIDErr
	}
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID && c.RotationID != nil && *c.RotationID == rotationID {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

func (f *fakeCredentialRepository) ListByPrincipal(_ context.Context, tenantID, principalID uuid.UUID) ([]*domain.Credential, error) {
	if f.listByPrincipalErr != nil {
		return nil, f.listByPrincipalErr
	}
	var out []*domain.Credential
	for _, c := range f.byID {
		if c.TenantID == tenantID && c.PrincipalID == principalID {
			cp := *c
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

func (f *fakeCredentialRepository) Insert(_ context.Context, c *domain.Credential) error {
	if f.forceInsertErr != nil {
		return f.forceInsertErr
	}
	if c.Status == domain.CredentialStatusActive {
		for _, existing := range f.byID {
			if existing.TenantID == c.TenantID && existing.PrincipalID == c.PrincipalID && existing.Status == domain.CredentialStatusActive {
				return f.rotationInFlight(existing)
			}
		}
	}
	for _, existing := range f.byID {
		if existing.TenantID == c.TenantID && existing.PrincipalID == c.PrincipalID && existing.Version == c.Version {
			return f.rotationInFlight(existing)
		}
	}
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	c.RecordVersion = 1
	if c.IssuedAt.IsZero() {
		// The real column defaults to now(); the replay window is measured
		// from it.
		c.IssuedAt = time.Now().UTC()
	}
	cp := *c
	f.byID[cp.ID] = &cp
	return nil
}

func (f *fakeCredentialRepository) rotationInFlight(active *domain.Credential) error {
	de := domain.NewError(domain.ErrRotationInFlight, "a concurrent issue/rotate is already in flight for this principal")
	if active.RotationID != nil {
		de = de.WithDetails(map[string]any{"active_rotation_id": *active.RotationID})
	}
	return de
}

func (f *fakeCredentialRepository) Update(_ context.Context, c *domain.Credential) error {
	if f.forceUpdateErr != nil {
		return f.forceUpdateErr
	}
	if f.raceOnUpdate != nil {
		race := f.raceOnUpdate
		f.raceOnUpdate = nil
		race(f.byID)
	}
	existing, ok := f.byID[c.ID]
	if !ok || existing.RecordVersion != c.RecordVersion {
		return domain.NewError(domain.ErrOptimisticLockConflict, "record_version conflict").
			WithDetails(map[string]any{"expected_version": c.RecordVersion})
	}
	c.RecordVersion = existing.RecordVersion + 1
	cp := *c
	f.byID[cp.ID] = &cp
	return nil
}

var _ port.CredentialRepository = (*fakeCredentialRepository)(nil)

type fakeSecretStore struct {
	data map[string]string

	// deleteErr, when set, is returned by Delete instead of succeeding —
	// lets a test drive CredentialService.sweepExpiredRotating's
	// revoke-failure warn() branch (via revokeCredential's secrets.Delete
	// call).
	deleteErr error

	// writeErr, when set, is returned by Write instead of succeeding — for
	// exercising issueOrRotate's OpenBao-write-failure branch (nothing
	// committed in Postgres, §9.3).
	writeErr error
}

func newFakeSecretStore() *fakeSecretStore {
	return &fakeSecretStore{data: map[string]string{}}
}

func (f *fakeSecretStore) Write(_ context.Context, path string, secret string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
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
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.data, path)
	return nil
}

func (f *fakeSecretStore) List(_ context.Context, pathPrefix string) ([]string, error) {
	var out []string
	for k := range f.data {
		out = append(out, k)
	}
	_ = pathPrefix
	return out, nil
}

// fakeLogger records every call for assertion — used to prove
// CredentialService.warn() actually fires (and with what message) rather
// than just not-crashing.
type fakeLogger struct {
	warnings []fakeLogCall
	infos    []fakeLogCall
}

type fakeLogCall struct {
	msg    string
	fields map[string]any
}

func (f *fakeLogger) Debug(string, map[string]any) {}
func (f *fakeLogger) Info(msg string, fields map[string]any) {
	f.infos = append(f.infos, fakeLogCall{msg: msg, fields: fields})
}
func (f *fakeLogger) Warn(msg string, fields map[string]any) {
	f.warnings = append(f.warnings, fakeLogCall{msg: msg, fields: fields})
}
func (f *fakeLogger) Error(string, map[string]any) {}

var _ port.Logger = (*fakeLogger)(nil)

var _ port.SecretStore = (*fakeSecretStore)(nil)

// LockByTenant behaves like ListByTenant in the fake (no real row locks).
func (f *fakePrincipalRepository) LockByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.ServiceAccountPrincipal, error) {
	return f.ListByTenant(ctx, tenantID)
}
