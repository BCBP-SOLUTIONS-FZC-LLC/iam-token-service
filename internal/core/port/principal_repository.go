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

	// LockForUpdate reads the principal like FindByID but takes a row lock
	// (SELECT … FOR UPDATE) held until the caller's transaction ends. TS-1,
	// TS-2 and the offboarding cascade take it first, so issue/rotate/revoke
	// for one principal are serialized: two concurrent TS-1 calls can no
	// longer pick the same next version and overwrite each other's OpenBao
	// material. MUST be called inside TxRunner.RunInTx. A lock wait that
	// exceeds the transaction's lock_timeout returns
	// domain.ErrRotationInFlight (409) — another write for this principal
	// is in progress.
	LockForUpdate(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error)

	// FindByPrincipalSub reads the principal by its Keycloak sub rather
	// than Token Service's own internal id — the TS-5 lookup (AUTH-9,
	// org-membership's service-account-not-grantable defense-in-depth
	// check). principal_sub is the same UUID shape callers see as a
	// human user's user_id, so this is the only way another service can
	// answer "is this subject a service account" without ever generating
	// principal_sub itself (TS-INV-1). Returns domain.ErrPrincipalNotFound
	// when absent or not visible under RLS (§5.5) — the expected, common
	// result for the overwhelming majority of calls, since almost every
	// subject checked is a real human user, not the tenant's automation
	// principal.
	FindByPrincipalSub(ctx context.Context, tenantID, principalSub uuid.UUID) (*domain.ServiceAccountPrincipal, error)

	// FindByType reads the tenant's principal of principalType
	// (uq_sap_active_principal — at most one per tenant) under the
	// caller's RLS-scoped tenant — the TS-6 lookup (TS-D17), the reverse
	// of FindByPrincipalSub: "which subject is tenant T's automation
	// principal". Returns domain.ErrPrincipalNotFound when the tenant has
	// none yet (not minted, or offboarded).
	FindByType(ctx context.Context, tenantID uuid.UUID, principalType domain.PrincipalType) (*domain.ServiceAccountPrincipal, error)

	// Register inserts p if no principal yet exists for
	// (tenant_id, principal_type) (uq_sap_active_principal); idempotent —
	// a repeat call with an unchanged principal_sub/keycloak_client_id
	// returns the existing row with created=false and updated=false (TS-4,
	// §5.4). A repeat call whose principal_sub or keycloak_client_id
	// differs from the stored row (RP-3 conversion carry-over — the
	// principal is re-registered against a newly-minted Keycloak client in
	// a dedicated realm) updates the row in place and returns
	// updated=true, rather than silently keeping the stale identity.
	Register(ctx context.Context, p *domain.ServiceAccountPrincipal) (result *domain.ServiceAccountPrincipal, created bool, updated bool, err error)

	// ListByTenant returns every principal row for tenantID (§8.4 —
	// normally 0 or 1 at MVP, uq_sap_active_principal; forward-compatible
	// with the post-launch multi-principal-type surface, §2.4). Used by
	// the offboarding cascade to discover every principal (and, via
	// CredentialRepository.ListByPrincipal, every OpenBao path) that must
	// be reclaimed before the tenant's rows are hard-deleted. A plain read,
	// no locks — also used by the public JWKS route.
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.ServiceAccountPrincipal, error)

	// LockByTenant is ListByTenant with the rows locked FOR UPDATE until the
	// caller's transaction ends — the offboarding cascade's read, so it
	// waits for (and then sees the result of) any in-progress issue/rotate,
	// which holds the same row lock, and no new one can start until the
	// cascade commits. MUST be called inside a transaction.
	LockByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.ServiceAccountPrincipal, error)

	// DeleteByTenant hard-deletes every principal row for tenantID; the
	// composite (principal_id, tenant_id) FK's ON DELETE CASCADE removes
	// its credential rows with it (§4.2, §8.4 offboarding cascade).
	DeleteByTenant(ctx context.Context, tenantID uuid.UUID) error
}
