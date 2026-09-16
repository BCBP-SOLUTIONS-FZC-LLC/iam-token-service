//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type credentialSummaryBody struct {
	Version     int     `json:"version"`
	Status      string  `json:"status"`
	OpenBaoPath string  `json:"openbao_path"`
	IssuedAt    string  `json:"issued_at"`
	ExpiresAt   *string `json:"expires_at,omitempty"`
}

type readPrincipalResponseBody struct {
	PrincipalID      uuid.UUID               `json:"principal_id"`
	TenantID         uuid.UUID               `json:"tenant_id"`
	KeycloakClientID string                  `json:"keycloak_client_id"`
	PrincipalType    string                  `json:"principal_type"`
	Status           string                  `json:"status"`
	RecordVersion    int                     `json:"record_version"`
	Credentials      []credentialSummaryBody `json:"credentials"`
}

// TestRead_JustRegisteredHasNoCredentials covers TS-3's happy path
// immediately after TS-4: the principal is found, with an empty (never
// nil-vs-null-ambiguous) credentials list.
func TestRead_JustRegisteredHasNoCredentials(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	principalID := registerPrincipal(t, tenantID)

	var body readPrincipalResponseBody
	resp := doJSON(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String(),
		sysHeaders(tenantID), nil, &body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body.PrincipalID != principalID {
		t.Fatalf("principal_id = %s, want %s", body.PrincipalID, principalID)
	}
	if len(body.Credentials) != 0 {
		t.Fatalf("credentials = %v, want empty", body.Credentials)
	}
}

// TestRead_AfterIssueShowsCredential covers TS-3 reflecting a credential
// issued via TS-1 — never the secret plaintext itself (§5.6).
func TestRead_AfterIssueShowsCredential(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	principalID := registerPrincipal(t, tenantID)

	if resp, _ := issueOrRotate(t, tenantID, principalID); resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue status = %d, want 201", resp.StatusCode)
	}

	var body readPrincipalResponseBody
	resp := doJSON(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String(),
		sysHeaders(tenantID), nil, &body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(body.Credentials) != 1 {
		t.Fatalf("credentials = %v, want exactly 1", body.Credentials)
	}
	if body.Credentials[0].Version != 1 {
		t.Fatalf("credentials[0].version = %d, want 1", body.Credentials[0].Version)
	}
	if body.Credentials[0].Status != "active" {
		t.Fatalf("credentials[0].status = %q, want active", body.Credentials[0].Status)
	}
}

// TestRead_NonexistentPrincipal covers the 404 path.
func TestRead_NonexistentPrincipal(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	resp, er := decodeErrorBody(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+uuid.NewString(),
		sysHeaders(tenantID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if er.Error != "principal_not_found" {
		t.Fatalf("error = %q, want principal_not_found", er.Error)
	}
}

// TestRead_MissingIdentityHeaders covers the 401 path when no identity
// headers are sent at all — see
// TestRegister_MissingIdentityHeaders/TestRegister_NonSystemPrincipalUserID
// (register_test.go) for why this asserts a generic gincommon error
// rather than this service's own missing_identity_headers code.
func TestRead_MissingIdentityHeaders(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	principalID := registerPrincipal(t, tenantID)

	resp, er := decodeErrorBody(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String(),
		nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if er.Error == "" {
		t.Fatal("error must be set")
	}
}

// TestRead_CrossTenantIsolation is the single most important scenario in
// this suite: a caller scoped to tenant B, given tenant A's own real
// principal id (so this can only be a genuine RLS gate, never an
// application-level "not registered yet" false negative), must get exactly
// the same 404 as a nonexistent principal — proving RLS actually hides the
// row at the database layer, not merely that the application chooses not
// to show it.
func TestRead_CrossTenantIsolation(t *testing.T) {
	t.Parallel()
	tenantA := newTenantID()
	tenantB := newTenantID()
	principalInA := registerPrincipal(t, tenantA)

	resp, er := decodeErrorBody(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantB.String()+"/service-accounts/"+principalInA.String(),
		sysHeaders(tenantB), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (RLS must hide tenant A's row from tenant B)", resp.StatusCode)
	}
	if er.Error != "principal_not_found" {
		t.Fatalf("error = %q, want principal_not_found", er.Error)
	}
}
