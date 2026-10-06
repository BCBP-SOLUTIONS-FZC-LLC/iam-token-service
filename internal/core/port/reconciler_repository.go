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

// CredentialMaterial is one credential row as the §8.6 reconciler sees it:
// its version, the OpenBao path recorded at issue time, and whether it is
// live (`active` or `rotating`). A revoked row is not live — its material
// is deleted by design (CUST-2), so its absence from OpenBao is expected.
type CredentialMaterial struct {
	Version     int
	OpenBaoPath string
	Live        bool
}

// PrincipalMaterialState is one principal's credential state, as seen
// cross-tenant by the §8.6 orphan-material reconciler: every credential row
// it has ever committed (Credentials, any status) and the highest version
// among them (MaxVersion, 0 when it has none). It is a lock-free snapshot:
// material no row claims is only an orphan candidate, which the reconciler
// deletes after re-checking under the principal's row lock (the lock TS-1
// holds across its OpenBao write and commit), so a concurrently-committing
// issue/rotate is never mistaken for an orphan. KeycloakClientID is the
// principal's CURRENT client id; older rows may live under a previous
// client id's prefix (an RP-3/RP-4 re-mint), which is why each row carries
// its path.
type PrincipalMaterialState struct {
	TenantID         uuid.UUID
	PrincipalID      uuid.UUID
	KeycloakClientID string
	MaxVersion       int
	Credentials      []CredentialMaterial
}

// DueForRotation is one `active` credential cmd/scheduler must rotate: its
// next_rotation_at has passed (§16 TSQ-6 Resolved, TS-D14, idx_sac_next_rotation).
type DueForRotation struct {
	TenantID    uuid.UUID
	PrincipalID uuid.UUID
	Version     int
}

// ReconcilerRepository implements the cross-tenant enumeration reads the
// §8.3/§8.6 cmd/rotator jobs and the §16 TSQ-6 cmd/scheduler job need.
// Every method MUST be called against the read-only, BYPASSRLS
// `serviceaccount_reconciler` pool (§4.3/RLS-7) — they scan across every
// tenant by design, which an RLS-scoped connection cannot do. Enumeration
// only: no method writes. Every resulting write (a revoke, or
// cmd/scheduler's IssueOrRotate call) goes through a separate RLS-scoped
// call bound to that row's own tenant.
type ReconcilerRepository interface {
	// ListExpiredRotating enumerates up to limit `rotating` credentials
	// across all tenants whose expires_at has passed, oldest first (§8.3,
	// idx_sac_overlap). A larger backlog is picked up by the next run.
	ListExpiredRotating(ctx context.Context, limit int) ([]ExpiredRotatingCredential, error)

	// ListPrincipalMaterialStates enumerates the principal registry across
	// all tenants with each principal's credential rows (§8.6) — the basis
	// for detecting orphaned OpenBao material (a version with no row,
	// confirmed under the principal lock before deletion) and missing
	// material (a LIVE row whose OpenBao entry is gone).
	ListPrincipalMaterialStates(ctx context.Context) ([]PrincipalMaterialState, error)

	// IsCredentialLive re-reads one credential's status at call time: the
	// reconciler confirms a missing-material candidate is still live (not
	// revoked since the snapshot) before paging on it.
	IsCredentialLive(ctx context.Context, principalID uuid.UUID, version int) (bool, error)

	// ListDueForRotation enumerates up to limit `active` credentials across
	// all tenants whose next_rotation_at has passed, most overdue first
	// (§16 TSQ-6 Resolved, idx_sac_next_rotation) — cmd/scheduler's due-list
	// scan. A larger backlog is picked up by the next run.
	ListDueForRotation(ctx context.Context, limit int) ([]DueForRotation, error)
}
