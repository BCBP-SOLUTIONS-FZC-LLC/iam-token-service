package main

// In-memory port fakes for cmd/scheduler's job unit tests. These live in
// package main (not under test/, TS's usual convention) because Go
// forbids importing package main from any other package — mirrors
// cmd/rotator/fakes_test.go's identical rationale.

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

type fakeReconcilerRepository struct {
	due []port.DueForRotation
	err error
}

func (f *fakeReconcilerRepository) ListExpiredRotating(context.Context) ([]port.ExpiredRotatingCredential, error) {
	return nil, nil
}

func (f *fakeReconcilerRepository) ListPrincipalMaterialStates(context.Context) ([]port.PrincipalMaterialState, error) {
	return nil, nil
}

func (f *fakeReconcilerRepository) ListDueForRotation(context.Context) ([]port.DueForRotation, error) {
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
}

func (f *fakeCredentialIssuer) IssueOrRotate(_ context.Context, _, principalID uuid.UUID, req service.IssueOrRotateRequest, _ uuid.UUID) (*service.IssueOrRotateResult, error) {
	f.calls = append(f.calls, principalID)
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
	refreshed map[uuid.UUID]int // tenantID -> number of RP-17 key-refresh calls
}

func newFakeRealmProvisionerClient() *fakeRealmProvisionerClient {
	return &fakeRealmProvisionerClient{errs: map[uuid.UUID]error{}, refreshed: map[uuid.UUID]int{}}
}

func (f *fakeRealmProvisionerClient) RefreshKeys(_ context.Context, tenantID uuid.UUID) error {
	if err, ok := f.errs[tenantID]; ok {
		return err
	}
	f.refreshed[tenantID]++
	return nil
}

var _ port.RealmProvisionerClient = (*fakeRealmProvisionerClient)(nil)

// newRevokedError/newRotationInFlightError build the two domain.Error
// codes rotateOneDue classifies as errAlreadyHandled (scan.go).
func newRevokedError() error {
	return domain.NewError(domain.ErrPrincipalRevoked, "principal is revoked")
}

func newRotationInFlightError() error {
	return domain.NewError(domain.ErrRotationInFlight, "a concurrent issue/rotate is already in flight")
}
