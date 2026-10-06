package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CredentialStatus is the ENUM `credential_status` (§4.1, frozen §25).
type CredentialStatus string

// CredentialStatus values (§4.2, §6.2). At most one `active` credential per
// principal (uq_sac_one_active, TS-INV-3); a `rotating` prior version may
// coexist during the bounded overlap window.
const (
	CredentialStatusActive   CredentialStatus = "active"
	CredentialStatusRotating CredentialStatus = "rotating"
	CredentialStatusRevoked  CredentialStatus = "revoked"
)

// Rotation-overlap bounds (§6.2, TS-CONFIG-4). overlap_seconds is clamped
// server-side to this range regardless of the request or a config default.
const (
	MinOverlapSeconds     = 0
	MaxOverlapSeconds     = 900
	DefaultOverlapSeconds = 300
)

// DefaultCadenceDays is the default rotation cadence (TS-D10, §16 TSQ-2
// Resolved) — the interval the cadence scheduler (cmd/scheduler, §16 TSQ-6
// Resolved) uses to compute next_rotation_at when no override is
// configured via ROTATION_DEFAULT_CADENCE_DAYS.
const DefaultCadenceDays = 90

// ClampOverlapSeconds clamps s to [MinOverlapSeconds, MaxOverlapSeconds]
// (§6.2, TS-CONFIG-4) — a hard server-side bound regardless of what the
// caller requests.
func ClampOverlapSeconds(s int) int {
	switch {
	case s < MinOverlapSeconds:
		return MinOverlapSeconds
	case s > MaxOverlapSeconds:
		return MaxOverlapSeconds
	default:
		return s
	}
}

// Credential is one row of `service_account_credentials` (§4.2) — a single
// versioned secret-material reference. The OpenBao path is a deterministic
// function of (tenant_id, keycloak_client_id, version); no secret is ever
// carried on this struct outside the transient TS-1 response (TS-INV-2).
type Credential struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	PrincipalID   uuid.UUID
	Version       int
	Status        CredentialStatus
	OpenBaoPath   string
	GrantedBy     uuid.UUID
	RotationID    *uuid.UUID
	RecordVersion int
	IssuedAt      time.Time
	UpdatedAt     time.Time
	RotatedAt     *time.Time
	ExpiresAt     *time.Time
	RevokedAt     *time.Time
	DeletedAt     *time.Time

	// RotationCadenceDays/NextRotationAt (§16 TSQ-6 Resolved) are set on
	// the 'active' row at issue/rotate time and cleared (nil) the moment
	// this row is demoted to 'rotating' or revoked — only the current
	// active version is ever "due". cmd/scheduler's due-list scan
	// (idx_sac_next_rotation) and TS-3 both read these.
	RotationCadenceDays *int
	NextRotationAt      *time.Time
}

// OpenBaoPathFor computes the deterministic KV v2 path for (tenantID,
// keycloakClientID, version) — frozen shape, §6.3/§25:
//
//	iam/serviceaccount/<tenant_id>/<keycloak_client_id>/v<version>
//
// Reconstructible without a Postgres read (CUST-3), so metadata/material
// divergence is always detectable.
func OpenBaoPathFor(tenantID uuid.UUID, keycloakClientID string, version int) string {
	return fmt.Sprintf("iam/serviceaccount/%s/%s/v%d", tenantID, keycloakClientID, version)
}

// OpenBaoPathPrefixFor is the parent of every version's OpenBao path for one
// client id — the prefix the §8.6 reconciler lists.
func OpenBaoPathPrefixFor(tenantID uuid.UUID, keycloakClientID string) string {
	return fmt.Sprintf("iam/serviceaccount/%s/%s", tenantID, keycloakClientID)
}

// OpenBaoTenantPrefix is the parent of every OpenBao path for one tenant —
// the subtree the offboarding cascade erases (§8.4/§15.2).
func OpenBaoTenantPrefix(tenantID uuid.UUID) string {
	return fmt.Sprintf("iam/serviceaccount/%s", tenantID)
}

// IsExpiredOverlap reports whether a 'rotating' credential's overlap window
// has closed as of now (§6.2/§8.3 — the sweep's revoke condition).
func (c *Credential) IsExpiredOverlap(now time.Time) bool {
	return c.Status == CredentialStatusRotating && c.ExpiresAt != nil && c.ExpiresAt.Before(now)
}
