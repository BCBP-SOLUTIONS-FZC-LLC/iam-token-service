package port

import (
	"context"

	"github.com/google/uuid"
)

// RealmProvisionerClient implements the RP-17 relay cmd/scheduler needs
// (§16 TSQ-6 Resolved, TS-D14). Under the EXT-6 client-jwt/JWKS mechanism
// (rev 1.3), TS-1 generates the credential and this service serves its
// public half as a per-tenant JWKS; the Realm Provisioner never receives
// key material (TS-INV-1 — this service never writes Keycloak). After a
// rotation, whichever caller drove TS-1 must still ask the Realm
// Provisioner to refresh Keycloak's cached JWKS for the tenant realm, so
// the new key is recognized and a removed one forgotten — Keycloak does
// not self-refresh a jwks.url client's key cache. An operator/O&M tool
// does this by hand today (§8.2); cmd/scheduler is the one automated
// caller that must do it itself.
type RealmProvisionerClient interface {
	// RefreshKeys calls RP-17 (POST
	// .../tenants/:id/service-account/keys/refresh, empty body) so Keycloak
	// re-fetches tenantID's platform-automation JWKS after a rotation. No key
	// material is passed — RP triggers Keycloak's clear-keys-cache, it does
	// not receive a secret (EXT-6/rev 1.3; replaces the removed ApplySecret
	// call against RP's deleted client-secret apply endpoint).
	RefreshKeys(ctx context.Context, tenantID uuid.UUID) error
}
