// Package domain holds entities, value objects, and domain errors for the
// Token Service. It imports nothing outside itself (§3.2).
package domain

import (
	"time"

	"github.com/google/uuid"
)

// PrincipalType is the ENUM `principal_type` (§4.1, frozen §25).
type PrincipalType string

// PrincipalType values. MVP ships only PrincipalTypePlatformAutomation;
// tenant_bot/user_pat are post-launch extensions of the same table (§2.4).
const (
	PrincipalTypePlatformAutomation PrincipalType = "platform_automation"
)

// PrincipalStatus is the ENUM `principal_status` (§4.1, frozen §25).
type PrincipalStatus string

// PrincipalStatus values (§4.2 — no "disabled" state; live or gone).
const (
	PrincipalStatusActive  PrincipalStatus = "active"
	PrincipalStatusRevoked PrincipalStatus = "revoked"
)

// KeycloakClientPlatformAutomation is the frozen Keycloak client name for
// the automation principal, minted by the Realm Provisioner, one per
// tenant (§25).
const KeycloakClientPlatformAutomation = "platform-automation"

// SystemPrincipalID is the reserved `iam-system` subject accepted only on
// /api/v1/internal/* (RLS-5, frozen §25/§5.2). It is the `granted_by`
// actor on cron- and Realm-Provisioner-initiated writes.
var SystemPrincipalID = uuid.MustParse("00000000-0000-0000-0000-0000000000a1")

// ServiceAccountPrincipal is one row of `service_account_principals` (§4.2)
// — the registry entry for a tenant's automation principal. principal_sub
// and keycloak_client_id are supplied by the Realm Provisioner (TS-4);
// this service never mints an identity (TS-INV-1).
type ServiceAccountPrincipal struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	PrincipalSub     uuid.UUID
	KeycloakClientID string
	PrincipalType    PrincipalType
	Status           PrincipalStatus
	RecordVersion    int
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// IsRevoked reports whether p rejects new issue/rotate (422
// principal_revoked, §4.2/§5.5).
func (p *ServiceAccountPrincipal) IsRevoked() bool {
	return p.Status == PrincipalStatusRevoked
}
