//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type registerRequestBody struct {
	PrincipalSub     string `json:"principal_sub"`
	KeycloakClientID string `json:"keycloak_client_id"`
}

type principalResponseBody struct {
	PrincipalID   uuid.UUID `json:"principal_id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	PrincipalType string    `json:"principal_type"`
	Status        string    `json:"status"`
	RecordVersion int       `json:"record_version"`
}

// TestRegister_FirstCallCreates covers TS-4's happy path: a first
// registration call returns 201 with a real principal id.
func TestRegister_FirstCallCreates(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	var body principalResponseBody
	resp := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID),
		registerRequestBody{PrincipalSub: uuid.NewString(), KeycloakClientID: "platform-automation"},
		&body,
	)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if body.PrincipalID == uuid.Nil {
		t.Fatal("principal_id must not be nil")
	}
	if body.TenantID != tenantID {
		t.Fatalf("tenant_id = %s, want %s", body.TenantID, tenantID)
	}
	if body.PrincipalType != "platform_automation" {
		t.Fatalf("principal_type = %q, want platform_automation", body.PrincipalType)
	}
	if body.Status != "active" {
		t.Fatalf("status = %q, want active", body.Status)
	}
}

// TestRegister_IdempotentRepeat covers §5.4: a repeat registration call for
// the same tenant returns 200 (not 201) with the SAME principal id — no
// second row is created.
func TestRegister_IdempotentRepeat(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	reqBody := registerRequestBody{PrincipalSub: uuid.NewString(), KeycloakClientID: "platform-automation"}

	var first principalResponseBody
	resp := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID), reqBody, &first)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201", resp.StatusCode)
	}

	var second principalResponseBody
	resp = doJSON(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID), reqBody, &second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repeat call status = %d, want 200", resp.StatusCode)
	}
	if second.PrincipalID != first.PrincipalID {
		t.Fatalf("repeat call returned a different principal_id: got %s, want %s", second.PrincipalID, first.PrincipalID)
	}
}

// TestRegister_MissingIdentityHeaders covers the 401 path when no identity
// headers are sent at all — rejected by gincommon's own RequireAuth
// middleware (platform-gincommon's generic "missing or invalid
// authentication headers"), before this service's own GUCBridgeMiddleware
// (and its missing_identity_headers code) ever runs. See
// TestRegister_NonSystemPrincipalUserID for the case that DOES reach this
// service's own check.
func TestRegister_MissingIdentityHeaders(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	resp, er := decodeErrorBody(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		map[string]string{"Content-Type": "application/json"},
		registerRequestBody{PrincipalSub: uuid.NewString(), KeycloakClientID: "platform-automation"},
	)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if er.Error == "" {
		t.Fatal("error must be set")
	}
}

// TestRegister_NonSystemPrincipalUserID covers §5.2's single authorization
// rule: headers are present and well-formed, but x-user-id is a real UUID
// that is NOT the reserved iam-system principal — this passes gincommon's
// own RequireAuth and reaches this service's own GUCBridgeMiddleware,
// which rejects it with the service's own missing_identity_headers code
// (internal/adapter/inbound/http/middleware.go).
func TestRegister_NonSystemPrincipalUserID(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	headers := sysHeaders(tenantID)
	headers["x-user-id"] = uuid.NewString() // any UUID other than domain.SystemPrincipalID

	resp, er := decodeErrorBody(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		headers, registerRequestBody{PrincipalSub: uuid.NewString(), KeycloakClientID: "platform-automation"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if er.Error != "missing_identity_headers" {
		t.Fatalf("error = %q, want missing_identity_headers", er.Error)
	}
}

// TestRegister_NonUUIDPrincipalSub covers the HTTP-layer "must be a UUID"
// validation on principal_sub (§10.3).
func TestRegister_NonUUIDPrincipalSub(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	resp, er := decodeErrorBody(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID),
		registerRequestBody{PrincipalSub: "not-a-uuid", KeycloakClientID: "platform-automation"},
	)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if er.Error != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", er.Error)
	}
	if er.Details["field"] != "principal_sub" {
		t.Fatalf("details.field = %v, want principal_sub", er.Details["field"])
	}
}

// TestRegister_WrongKeycloakClientID covers the service-layer (§10.3)
// validation that keycloak_client_id must be the frozen
// "platform-automation" value at MVP.
func TestRegister_WrongKeycloakClientID(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	resp, er := decodeErrorBody(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID),
		registerRequestBody{PrincipalSub: uuid.NewString(), KeycloakClientID: "some-other-client"},
	)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if er.Error != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", er.Error)
	}
	if er.Details["field"] != "keycloak_client_id" {
		t.Fatalf("details.field = %v, want keycloak_client_id", er.Details["field"])
	}
}
