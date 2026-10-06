// Package main is the entry point for the IAM Token Service.
//
// @title           IAM Token Service API
// @version         1.0
// @description     Token Service microservice — custodian of the platform-automation service account's rotating credential material (issue, rotate, revoke; TS-1..TS-6 plus the EXT-6 JWKS route).
// @description
// @description     **Tenant isolation:** All resource access is strictly scoped by x-tenant-id. Cross-tenant access is never permitted (RLS-6).
// @description     **Secret handling:** The generated credential plaintext is returned exactly once, on TS-1's response body, and never logged, traced, or echoed again (TS-INV-2).
// @description     **No Keycloak dependency:** This service manages its own credential material independently — it never calls the Keycloak Admin API (TS-INV-1).
//
// @contact.name   BCBP Solutions IAM Team
// @contact.email  platform@bcbpsolutions.com
//
// @license.name   Proprietary
//
// @host      localhost:8080
// @BasePath  /api/v1/internal
//
// @securityDefinitions.apikey TenantID
// @in                         header
// @name                       x-tenant-id
// @description                Tenant UUID injected by the API gateway. Must match the :id path segment (§5.1).
//
// @securityDefinitions.apikey UserID
// @in                         header
// @name                       x-user-id
// @description                Must be the fixed iam-system principal UUID 00000000-0000-0000-0000-0000000000a1 (domain.SystemPrincipalID); anything else is 401 missing_identity_headers.
//
// @tag.name         ServiceAccounts
// @tag.description  Service-account principal registration and metadata read (TS-3, TS-4)
//
// @tag.name         Credentials
// @tag.description  Credential lifecycle — issue, rotate, revoke (TS-1, TS-2)
//
// @tag.name         Infra
// @tag.description  Health and readiness probes
package main
