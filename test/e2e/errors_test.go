//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestTenantPathMismatch covers RequireTenantPathMatch (§5.1): the
// x-tenant-id header must equal the :id path segment, or the request is
// rejected before ever reaching a repository — a 403, not a 404 (this is
// an application-level guard in front of RLS, not RLS itself).
func TestTenantPathMismatch(t *testing.T) {
	t.Parallel()
	pathTenant := newTenantID()
	headerTenant := newTenantID() // deliberately different

	resp, _ := doRequest(t, http.MethodGet,
		"/api/v1/internal/tenants/"+pathTenant.String()+"/service-accounts/"+uuid.NewString(),
		map[string]string{
			"x-user-id":      "00000000-0000-0000-0000-0000000000a1",
			"x-tenant-id":    headerTenant.String(),
			"x-tenant-roles": "iam-system",
		}, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// TestUnsupportedMediaType covers RequireJSONContentType (§5.1): a POST
// carrying a non-empty body without application/json is rejected with 415,
// before any handler logic runs.
func TestUnsupportedMediaType(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	headers := sysHeaders(tenantID)
	headers["Content-Type"] = "text/plain"

	resp, body := doRequest(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		headers, []byte(`{"principal_sub":"`+uuid.NewString()+`","keycloak_client_id":"platform-automation"}`))
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415, body=%s", resp.StatusCode, body)
	}
}

// TestMalformedJSONBody covers the 400 invalid_request path for a body
// that isn't valid JSON at all.
func TestMalformedJSONBody(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	resp, body := doRequest(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID), []byte(`{not valid json`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", resp.StatusCode, body)
	}
}
