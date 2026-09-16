package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/google/uuid"
)

// PrincipalRepository persists `service_account_principals` (§4.2). Every
// method binds the RLS GUC transaction-locally via the caller's ctx
// (RLS-6).
type PrincipalRepository interface {
	// FindByID reads the principal (tenantID, principalID) under the
	// caller's RLS-scoped tenant — the TS-1/TS-2/TS-3 path lookup. Returns
	// domain.ErrPrincipalNotFound when absent or not visible under RLS
	// (§5.5).
	FindByID(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error)

	// Register inserts p if no principal yet exists for
	// (tenant_id, principal_type) (uq_sap_active_principal); idempotent —
	// a repeat call returns the existing row and created=false rather than
	// erroring (TS-4, §5.4).
	Register(ctx context.Context, p *domain.ServiceAccountPrincipal) (result *domain.ServiceAccountPrincipal, created bool, err error)

	// ListByTenant returns every principal row for tenantID (§8.4 —
	// normally 0 or 1 at MVP, uq_sap_active_principal; forward-compatible
	// with the post-launch multi-principal-type surface, §2.4). Used by
	// the offboarding cascade to discover every principal (and, via
	// CredentialRepository.ListByPrincipal, every OpenBao path) that must
	// be reclaimed before the tenant's rows are hard-deleted.
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.ServiceAccountPrincipal, error)

	// DeleteByTenant hard-deletes every principal row for tenantID; the
	// composite (principal_id, tenant_id) FK's ON DELETE CASCADE removes
	// its credential rows with it (§4.2, §8.4 offboarding cascade).
	DeleteByTenant(ctx context.Context, tenantID uuid.UUID) error
}
