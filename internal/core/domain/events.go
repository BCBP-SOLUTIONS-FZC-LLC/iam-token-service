package domain

import (
	"github.com/google/uuid"
)

// Event-type names (§7.5, frozen §25). PascalCase — this is the Glue schema
// name, the AsyncAPI message definition, the EventType SNS attribute name
// and value, and the envelope `type` field: one naming form, no
// translation (§7.3.1).
const (
	EventServiceAccountRegistered        = "ServiceAccountRegistered"
	EventServiceAccountCredentialIssued  = "ServiceAccountCredentialIssued"
	EventServiceAccountCredentialRotated = "ServiceAccountCredentialRotated"
	EventServiceAccountCredentialRevoked = "ServiceAccountCredentialRevoked"
	EventServiceAccountRevoked           = "ServiceAccountRevoked"
)

// Event is a domain-level event bound for the outbox. Actor is the
// x-user-id of the caller that triggered the transition (the reserved
// system principal on cron/RP paths, an operator id otherwise, §4.2).
type Event struct {
	Type     string
	TenantID uuid.UUID
	Actor    uuid.UUID
	Data     any
}

// ServiceAccountRegisteredPayload — emitted by TS-4 (§7.5).
type ServiceAccountRegisteredPayload struct {
	TenantID         uuid.UUID `json:"tenant_id"`
	PrincipalID      uuid.UUID `json:"principal_id"`
	PrincipalSub     uuid.UUID `json:"principal_sub"`
	KeycloakClientID string    `json:"keycloak_client_id"`
	PrincipalType    string    `json:"principal_type"`
	CreatedAt        string    `json:"created_at"`
}

// ServiceAccountCredentialIssuedPayload — emitted by TS-1 on the
// first-ever credential for a principal (version = 1, §7.5).
type ServiceAccountCredentialIssuedPayload struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	PrincipalID uuid.UUID `json:"principal_id"`
	Version     int       `json:"version"`
	IssuedAt    string    `json:"issued_at"`
}

// ServiceAccountCredentialRotatedPayload — emitted by TS-1 on rotation
// (version > 1, §7.5).
type ServiceAccountCredentialRotatedPayload struct {
	TenantID       uuid.UUID `json:"tenant_id"`
	PrincipalID    uuid.UUID `json:"principal_id"`
	Version        int       `json:"version"`
	PriorVersion   int       `json:"prior_version"`
	ExpiresPriorAt string    `json:"expires_prior_at"`
}

// ServiceAccountCredentialRevokedPayload — emitted by TS-2, the
// overlap-expiry sweep, or the offboarding cascade (§7.5).
type ServiceAccountCredentialRevokedPayload struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	PrincipalID uuid.UUID `json:"principal_id"`
	Version     int       `json:"version"`
	RevokedAt   string    `json:"revoked_at"`
}

// ServiceAccountRevokedPayload — emitted when a principal is fully revoked
// (offboarding, §7.5).
type ServiceAccountRevokedPayload struct {
	TenantID    uuid.UUID `json:"tenant_id"`
	PrincipalID uuid.UUID `json:"principal_id"`
	RevokedAt   string    `json:"revoked_at"`
}
