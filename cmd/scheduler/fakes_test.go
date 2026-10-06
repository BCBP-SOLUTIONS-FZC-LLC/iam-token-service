package main

// In-memory port fakes for cmd/scheduler's job unit tests. These live in
// package main (not under test/, TS's usual convention) because Go
// forbids importing package main from any other package — mirrors
// cmd/rotator/fakes_test.go's identical rationale.

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

type fakeReconcilerRepository struct {
	due []port.DueForRotation
	err error
}

func (f *fakeReconcilerRepository) ListExpiredRotating(context.Context, int) ([]port.ExpiredRotatingCredential, error) {
	return nil, nil
}

func (f *fakeReconcilerRepository) ListPrincipalMaterialStates(context.Context) ([]port.PrincipalMaterialState, error) {
	return nil, nil
}

// IsCredentialLive is unused by cmd/scheduler; stubbed to satisfy the port.
func (f *fakeReconcilerRepository) IsCredentialLive(context.Context, uuid.UUID, int) (bool, error) {
	return false, nil
}

func (f *fakeReconcilerRepository) ListDueForRotation(context.Context, int) ([]port.DueForRotation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.due, nil
}

var _ port.ReconcilerRepository = (*fakeReconcilerRepository)(nil)

// fakeCredentialIssuer stubs credentialIssuer (scan.go) — a per-principal
// canned result/error keyed by principalID, so a test can make one
// principal fail while others succeed in the same run.
type fakeCredentialIssuer struct {
	results map[uuid.UUID]*service.IssueOrRotateResult
	errs    map[uuid.UUID]error
	calls   []uuid.UUID // principalIDs called, in order — asserts a per-row rotation_id was generated fresh
	overlap []int       // OverlapSeconds of each call
	expect  []int       // ExpectActiveVersion of each call

	// onIssue runs at the start of every call — a test uses it to cancel
	// the run context (SIGTERM) mid-rotation or to inspect the marker.
	onIssue func(principalID uuid.UUID)
}

func (f *fakeCredentialIssuer) IssueOrRotate(_ context.Context, _, principalID uuid.UUID, req service.IssueOrRotateRequest, _ uuid.UUID) (*service.IssueOrRotateResult, error) {
	f.calls = append(f.calls, principalID)
	f.overlap = append(f.overlap, req.OverlapSeconds)
	f.expect = append(f.expect, req.ExpectActiveVersion)
	if f.onIssue != nil {
		f.onIssue(principalID)
	}
	if err, ok := f.errs[principalID]; ok {
		return nil, err
	}
	if res, ok := f.results[principalID]; ok {
		return res, nil
	}
	return &service.IssueOrRotateResult{Version: 1, Secret: "generated-secret", RecordVersion: 1}, nil
}

var _ credentialIssuer = (*fakeCredentialIssuer)(nil)

// fakeRealmProvisionerClient stubs port.RealmProvisionerClient — an
// optional per-tenant error lets a test fail exactly one RP-17
// key-refresh without affecting the rest of the run.
type fakeRealmProvisionerClient struct {
	errs      map[uuid.UUID]error
	refreshed map[uuid.UUID]int // tenantID -> number of successful RP-17 key-refresh calls
	order     []uuid.UUID       // every call's tenantID, in order
	ctxErrs   []error           // ctx.Err() observed at each call — must be nil even after the run ctx is cancelled
	onRefresh func(tenantID uuid.UUID)
}

func newFakeRealmProvisionerClient() *fakeRealmProvisionerClient {
	return &fakeRealmProvisionerClient{errs: map[uuid.UUID]error{}, refreshed: map[uuid.UUID]int{}}
}

func (f *fakeRealmProvisionerClient) RefreshKeys(ctx context.Context, tenantID uuid.UUID) error {
	f.order = append(f.order, tenantID)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.onRefresh != nil {
		f.onRefresh(tenantID)
	}
	if err, ok := f.errs[tenantID]; ok {
		return err
	}
	f.refreshed[tenantID]++
	return nil
}

// fakeKeysRefreshMarkers is an in-memory keys_refresh_pending, including
// the intent/committed distinction (intent_until): an intent marker is
// hidden from ListPending (as a live intent is in Postgres), MarkPending
// turns it committed, and MarkIntent never downgrades a committed one.
type fakeKeysRefreshMarkers struct {
	mu      sync.Mutex
	clock   time.Time
	pending map[uuid.UUID]time.Time
	intent  map[uuid.UUID]bool // true while the tenant's marker is a live intent
	marks   int
	markErr error // MarkIntent (the pre-rotation write) fails

	commitMarks   int
	commitMarkErr error                    // MarkPending (the committed re-mark) fails
	onCommitMark  func(tenantID uuid.UUID) // runs before every MarkPending, outside the lock
	intentTTLs    []time.Duration
}

func newFakeKeysRefreshMarkers() *fakeKeysRefreshMarkers {
	return &fakeKeysRefreshMarkers{clock: time.Unix(1_700_000_000, 0), pending: map[uuid.UUID]time.Time{}, intent: map[uuid.UUID]bool{}}
}

func (f *fakeKeysRefreshMarkers) MarkIntent(_ context.Context, tenantID uuid.UUID, ttl time.Duration) (pgadapter.KeysRefreshMark, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return pgadapter.KeysRefreshMark{}, f.markErr
	}
	f.marks++
	f.intentTTLs = append(f.intentTTLs, ttl)
	f.clock = f.clock.Add(time.Second)
	_, existed := f.pending[tenantID]
	if !existed {
		f.intent[tenantID] = true
	} // an existing committed marker is never downgraded
	f.pending[tenantID] = f.clock
	return pgadapter.KeysRefreshMark{RequestedAt: f.clock, Fresh: !existed}, nil
}

func (f *fakeKeysRefreshMarkers) MarkPending(_ context.Context, tenantID uuid.UUID) (pgadapter.KeysRefreshMark, error) {
	if f.onCommitMark != nil {
		f.onCommitMark(tenantID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commitMarkErr != nil {
		return pgadapter.KeysRefreshMark{}, f.commitMarkErr
	}
	f.commitMarks++
	f.clock = f.clock.Add(time.Second)
	_, existed := f.pending[tenantID]
	f.pending[tenantID] = f.clock
	delete(f.intent, tenantID)
	return pgadapter.KeysRefreshMark{RequestedAt: f.clock, Fresh: !existed}, nil
}

func (f *fakeKeysRefreshMarkers) Clear(_ context.Context, tenantID uuid.UUID, upTo time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if at, ok := f.pending[tenantID]; ok && !at.After(upTo) {
		delete(f.pending, tenantID)
		delete(f.intent, tenantID)
	}
	return nil
}

func (f *fakeKeysRefreshMarkers) ListPending(context.Context, int) ([]pgadapter.PendingKeysRefresh, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []pgadapter.PendingKeysRefresh
	for id, at := range f.pending {
		if f.intent[id] {
			continue // a live intent is left to its writer
		}
		out = append(out, pgadapter.PendingKeysRefresh{TenantID: id, RequestedAt: at, HasPrincipal: true})
	}
	return out, nil
}

// expireIntents simulates every intent_until passing.
func (f *fakeKeysRefreshMarkers) expireIntents() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intent = map[uuid.UUID]bool{}
}

func (f *fakeKeysRefreshMarkers) isPending(tenantID uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.pending[tenantID]
	return ok
}

func (f *fakeKeysRefreshMarkers) isIntent(tenantID uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.intent[tenantID]
}

var _ keysRefreshMarkers = (*fakeKeysRefreshMarkers)(nil)

var _ port.RealmProvisionerClient = (*fakeRealmProvisionerClient)(nil)

// newRevokedError/newRotationInFlightError build the two domain.Error
// codes rotateOneDue classifies as errAlreadyHandled (scan.go).
func newRevokedError() error {
	return domain.NewError(domain.ErrPrincipalRevoked, "principal is revoked")
}

func newRotationInFlightError() error {
	return domain.NewError(domain.ErrRotationInFlight, "a concurrent issue/rotate is already in flight")
}
