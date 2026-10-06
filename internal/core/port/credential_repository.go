package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/google/uuid"
)

// CredentialRepository persists `service_account_credentials` (§4.2).
// Every method binds the RLS GUC transaction-locally via the caller's ctx
// (RLS-6) — cross-tenant enumeration for the §8.3/§8.6 cron jobs is a
// separate, deliberately narrower concern (RLS-7) implemented directly on
// the reconciler pool, not through this port.
type CredentialRepository interface {
	// FindByVersion reads one credential version (tenantID, principalID,
	// version) under the caller's RLS-scoped tenant — TS-2's revoke target
	// lookup.
	FindByVersion(ctx context.Context, tenantID, principalID uuid.UUID, version int) (*domain.Credential, error)

	// FindActive returns the principal's current `active` credential, if
	// any (§6.2 — at most one at a time, uq_sac_one_active).
	FindActive(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.Credential, error)

	// MaxVersion returns the highest version ever committed for the
	// principal, whatever its status (0 when none). TS-1 issues max+1: the
	// active row's version is not enough once the active version has been
	// revoked (uq_sac_version would reject every later issue).
	MaxVersion(ctx context.Context, tenantID, principalID uuid.UUID) (int, error)

	// FindByRotationID returns the credential row already created for
	// (principalID, rotationID), if any — TS-1's idempotency check
	// (uq_sac_rotation_id, §9.2).
	FindByRotationID(ctx context.Context, tenantID, principalID, rotationID uuid.UUID) (*domain.Credential, error)

	// ListByPrincipal returns every version for principalID, newest first
	// (idx_sac_principal) — TS-3's metadata read (§8.5).
	ListByPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) ([]*domain.Credential, error)

	// Insert creates a new credential row (an issue, or the new `active`
	// version of a rotation).
	Insert(ctx context.Context, c *domain.Credential) error

	// Update applies an optimistic-lock-guarded status transition (WHERE
	// id=$1 AND record_version=$2) — demote-to-rotating, revoke, and the
	// §8.3 sweep's expiry-revoke all go through this one method. Returns
	// domain.ErrOptimisticLockConflict on a stale record_version.
	Update(ctx context.Context, c *domain.Credential) error
}
