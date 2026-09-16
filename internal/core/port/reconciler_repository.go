package port

import (
	"context"

	"github.com/google/uuid"
)

// ExpiredRotatingCredential is one row the §8.3 overlap-expiry sweep must
// revoke: a `rotating` credential whose `expires_at` has passed.
type ExpiredRotatingCredential struct {
	TenantID    uuid.UUID
	PrincipalID uuid.UUID
	Version     int
	OpenBaoPath string
}

// PrincipalMaterialState is one principal's committed-credential state, as
// seen cross-tenant by the §8.6 orphan-material reconciler: every version
// it has ever committed to Postgres (CommittedVersions) and the highest of
// those (MaxVersion) — the boundary the reconciler uses to distinguish a
// safely-reclaimable orphan (version < MaxVersion) from a version that may
// belong to a concurrently-committing issue/rotate (version >= MaxVersion,
// left for the next run).
type PrincipalMaterialState struct {
	TenantID          uuid.UUID
	PrincipalID       uuid.UUID
	KeycloakClientID  string
	MaxVersion        int
	CommittedVersions []int
}

// ReconcilerRepository implements the two cross-tenant enumeration reads
// the §8.3/§8.6 cmd/rotator jobs need. Both methods MUST be called against
// the read-only, BYPASSRLS `serviceaccount_reconciler` pool (§4.3/RLS-7) —
// they scan across every tenant by design, which an RLS-scoped connection
// cannot do. Enumeration only: neither method writes. Every resulting
// write (a revoke) goes through a separate RLS-scoped CredentialRepository
// call bound to that row's own tenant.
type ReconcilerRepository interface {
	// ListExpiredRotating enumerates every `rotating` credential across all
	// tenants whose expires_at has passed (§8.3, idx_sac_overlap).
	ListExpiredRotating(ctx context.Context) ([]ExpiredRotatingCredential, error)

	// ListPrincipalMaterialStates enumerates the principal registry across
	// all tenants with each principal's committed credential versions
	// (§8.6) — the basis for detecting orphaned OpenBao material (a
	// version below MaxVersion with no committed row) and missing material
	// (a committed version whose OpenBao entry is gone).
	ListPrincipalMaterialStates(ctx context.Context) ([]PrincipalMaterialState, error)
}
