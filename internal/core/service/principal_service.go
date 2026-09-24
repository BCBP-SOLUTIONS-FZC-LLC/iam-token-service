package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// PrincipalService implements TS-4 (register), TS-3 (read metadata),
// TS-5 (find by sub) and TS-6 (read the automation principal) (§5.4,
// §8.1, §8.5).
type PrincipalService struct {
	principals  port.PrincipalRepository
	credentials port.CredentialRepository
	tx          port.TxRunner
}

// NewPrincipalService wires PrincipalService's dependencies.
func NewPrincipalService(principals port.PrincipalRepository, credentials port.CredentialRepository, tx port.TxRunner) *PrincipalService {
	return &PrincipalService{principals: principals, credentials: credentials, tx: tx}
}

// RegisterRequest is the TS-4 request body (§5.4). Supplied by the Realm
// Provisioner after it mints the `platform-automation` Keycloak client;
// this service never generates principal_sub (TS-INV-1).
type RegisterRequest struct {
	PrincipalSub     uuid.UUID
	KeycloakClientID string
}

// RegisterResult is the TS-4 response body (§5.4). Created is false on an
// idempotent repeat (§5.4 — 200 rather than 201).
type RegisterResult struct {
	PrincipalID   uuid.UUID
	TenantID      uuid.UUID
	PrincipalType domain.PrincipalType
	Status        domain.PrincipalStatus
	RecordVersion int
	Created       bool
}

// Register implements TS-4 (§5.4, §8.1): idempotent create on
// (tenant_id, principal_type) — a repeat call returns the existing
// principal rather than erroring. principal_sub is stored verbatim
// (RP-supplied, never generated here); keycloak_client_id is validated
// against the frozen platform-automation value at MVP (§10.3).
func (s *PrincipalService) Register(ctx context.Context, tenantID uuid.UUID, req RegisterRequest, actor uuid.UUID) (*RegisterResult, error) {
	if req.PrincipalSub == uuid.Nil {
		return nil, domain.NewError(domain.ErrInvalidRequest, "principal_sub is required").
			WithDetails(map[string]any{"field": "principal_sub"})
	}
	if !domain.ValidPlatformAutomationClientID(req.KeycloakClientID, tenantID) {
		return nil, domain.NewError(domain.ErrInvalidRequest, "keycloak_client_id must be 'platform-automation' or 'platform-automation-<tenant_id>' at MVP").
			WithDetails(map[string]any{"field": "keycloak_client_id"})
	}

	candidate := &domain.ServiceAccountPrincipal{
		TenantID: tenantID, PrincipalSub: req.PrincipalSub, KeycloakClientID: req.KeycloakClientID,
		PrincipalType: domain.PrincipalTypePlatformAutomation,
	}

	var result *domain.ServiceAccountPrincipal
	var created, updated bool
	err := s.tx.RunInTx(ctx, func(ctx context.Context) error {
		var err error
		result, created, updated, err = s.principals.Register(ctx, candidate)
		if err != nil {
			return err
		}
		if !created && !updated {
			return nil
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventServiceAccountRegistered, TenantID: tenantID, Actor: actor,
			Data: domain.ServiceAccountRegisteredPayload{
				TenantID: tenantID, PrincipalID: result.ID, PrincipalSub: result.PrincipalSub,
				KeycloakClientID: result.KeycloakClientID, PrincipalType: string(result.PrincipalType),
				CreatedAt: result.CreatedAt.UTC().Format(time.RFC3339),
			},
		})
	})
	if err != nil {
		return nil, err
	}

	return &RegisterResult{
		PrincipalID: result.ID, TenantID: result.TenantID, PrincipalType: result.PrincipalType,
		Status: result.Status, RecordVersion: result.RecordVersion, Created: created,
	}, nil
}

// FindBySubResult is the TS-5 response body (AUTH-9) — deliberately lighter
// than ReadPrincipalResult: callers checking "is this subject a service
// account" need identity/status only, never credential metadata.
type FindBySubResult struct {
	PrincipalID   uuid.UUID
	TenantID      uuid.UUID
	PrincipalType domain.PrincipalType
	Status        domain.PrincipalStatus
	RecordVersion int
}

// FindPrincipalBySub implements TS-5 (AUTH-9): looks up a principal by its
// Keycloak sub rather than Token Service's own internal id — the signal
// org-membership's service-account-not-grantable defense-in-depth check
// needs, since a subject's Keycloak sub is the only identifier it ever
// sees (as user_id) and this service never generates principal_sub itself
// (TS-INV-1). Returns domain.ErrPrincipalNotFound when absent — expected
// for the overwhelming majority of calls (real human users, not the
// tenant's automation principal).
func (s *PrincipalService) FindPrincipalBySub(ctx context.Context, tenantID, principalSub uuid.UUID) (*FindBySubResult, error) {
	p, err := s.principals.FindByPrincipalSub(ctx, tenantID, principalSub)
	if err != nil {
		return nil, err
	}
	return &FindBySubResult{
		PrincipalID: p.ID, TenantID: p.TenantID,
		PrincipalType: p.PrincipalType, Status: p.Status, RecordVersion: p.RecordVersion,
	}, nil
}

// AutomationPrincipalResult is the TS-6 response body (TS-D17): the
// tenant's platform-automation identity — the subject a caller names as
// the acting principal — without credential metadata (TS-3 serves that).
type AutomationPrincipalResult struct {
	PrincipalID      uuid.UUID
	TenantID         uuid.UUID
	PrincipalSub     uuid.UUID
	KeycloakClientID string
	PrincipalType    domain.PrincipalType
	Status           domain.PrincipalStatus
	RecordVersion    int
}

// ReadPlatformAutomation implements TS-6 (TS-D17): resolves tenant T's
// platform-automation principal, so a caller that has only a tenant id (a
// Workflow connector worker naming the acting principal on a callback)
// can learn its Keycloak sub. The reverse of FindPrincipalBySub. The sub
// is stable across credential rotation (TS-1 never touches the principal
// row) but NOT across an RP-3 convert / RP-4 revert re-mint, which
// re-registers the same principal_id against a new sub (TS-4, §5.4) —
// principal_id is the identity that survives. Returns
// domain.ErrPrincipalNotFound when the tenant has none.
func (s *PrincipalService) ReadPlatformAutomation(ctx context.Context, tenantID uuid.UUID) (*AutomationPrincipalResult, error) {
	p, err := s.principals.FindByType(ctx, tenantID, domain.PrincipalTypePlatformAutomation)
	if err != nil {
		return nil, err
	}
	return &AutomationPrincipalResult{
		PrincipalID: p.ID, TenantID: p.TenantID, PrincipalSub: p.PrincipalSub,
		KeycloakClientID: p.KeycloakClientID, PrincipalType: p.PrincipalType,
		Status: p.Status, RecordVersion: p.RecordVersion,
	}, nil
}

// CredentialSummary is one entry in ReadPrincipalResult.Credentials (§5.4
// TS-3) — metadata only, never a secret.
type CredentialSummary struct {
	Version     int
	Status      domain.CredentialStatus
	OpenBaoPath string
	IssuedAt    time.Time
	ExpiresAt   *time.Time

	// RotationCadenceDays/NextRotationAt (§16 TSQ-6 Resolved) are non-nil
	// only on the `active` entry — cleared on every superseded version
	// (§4.2), so O&M sees exactly one "when is this due" answer per
	// principal.
	RotationCadenceDays *int
	NextRotationAt      *time.Time
}

// ReadPrincipalResult is the TS-3 response body (§5.4).
type ReadPrincipalResult struct {
	PrincipalID      uuid.UUID
	TenantID         uuid.UUID
	KeycloakClientID string
	PrincipalType    domain.PrincipalType
	Status           domain.PrincipalStatus
	RecordVersion    int
	Credentials      []CredentialSummary
}

// ReadPrincipal implements TS-3 (§5.4, §8.5): the principal plus its full
// credential-version list, newest first — metadata only, never touches
// OpenBao and never returns a secret (§5.6). This service's only
// steady-state read path (§21).
func (s *PrincipalService) ReadPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) (*ReadPrincipalResult, error) {
	p, err := s.principals.FindByID(ctx, tenantID, principalID)
	if err != nil {
		return nil, err
	}
	creds, err := s.credentials.ListByPrincipal(ctx, tenantID, principalID)
	if err != nil {
		return nil, err
	}
	summaries := make([]CredentialSummary, 0, len(creds))
	for _, c := range creds {
		summaries = append(summaries, CredentialSummary{
			Version: c.Version, Status: c.Status, OpenBaoPath: c.OpenBaoPath,
			IssuedAt: c.IssuedAt, ExpiresAt: c.ExpiresAt,
			RotationCadenceDays: c.RotationCadenceDays, NextRotationAt: c.NextRotationAt,
		})
	}
	return &ReadPrincipalResult{
		PrincipalID: p.ID, TenantID: p.TenantID, KeycloakClientID: p.KeycloakClientID,
		PrincipalType: p.PrincipalType, Status: p.Status, RecordVersion: p.RecordVersion,
		Credentials: summaries,
	}, nil
}
