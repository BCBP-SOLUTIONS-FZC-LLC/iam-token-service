//go:build e2e

package e2e_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
)

type revokePrincipalResponseBody struct {
	PrincipalID          uuid.UUID `json:"principal_id"`
	TenantID             uuid.UUID `json:"tenant_id"`
	KeycloakClientID     string    `json:"keycloak_client_id"`
	Status               string    `json:"status"`
	RecordVersion        int       `json:"record_version"`
	RevokedAt            string    `json:"revoked_at"`
	RevokedVersions      []int     `json:"revoked_versions"`
	KeycloakInvalidation string    `json:"keycloak_invalidation"`
}

func revokePrincipalPath(tenantID uuid.UUID) string {
	return "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/platform-automation/revoke"
}

// TestRevokePrincipal_EndToEnd covers TS-7 through the real router and
// middleware chain: register, issue, revoke the principal (bodyless POST, as
// the Realm Provisioner sends it), repeat for the same body, then TS-1 is
// refused.
func TestRevokePrincipal_EndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tenantID := newTenantID()
	headers := sysHeaders(tenantID)
	delete(headers, "Content-Type")

	if resp, p := decodeErrorBody(t, http.MethodPost, revokePrincipalPath(tenantID), headers, nil); resp.StatusCode != http.StatusNotFound || p.ErrorCode != "PRINCIPAL_NOT_FOUND" {
		t.Fatalf("no principal: status %d errorCode %q, want 404 PRINCIPAL_NOT_FOUND", resp.StatusCode, p.ErrorCode)
	}

	principalID := registerPrincipal(t, tenantID)
	if resp, _ := issueOrRotate(t, tenantID, principalID); resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue: status %d", resp.StatusCode)
	}

	var first revokePrincipalResponseBody
	if resp := doJSON(t, http.MethodPost, revokePrincipalPath(tenantID), headers, nil, &first); resp.StatusCode != http.StatusOK {
		t.Fatalf("TS-7: status %d, want 200", resp.StatusCode)
	}
	if first.PrincipalID != principalID || first.Status != "revoked" || !reflect.DeepEqual(first.RevokedVersions, []int{1}) {
		t.Fatalf("TS-7 body = %+v", first)
	}
	var again revokePrincipalResponseBody
	if resp := doJSON(t, http.MethodPost, revokePrincipalPath(tenantID), headers, nil, &again); resp.StatusCode != http.StatusOK {
		t.Fatalf("TS-7 repeat: status %d, want 200", resp.StatusCode)
	}
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("TS-7 repeat body = %+v, want %+v", again, first)
	}
	if n := outboxEventCount(t, ctx, domain.EventServiceAccountRevoked, tenantID); n != 1 {
		t.Fatalf("ServiceAccountRevoked events = %d, want 1", n)
	}
	if n := outboxEventCount(t, ctx, domain.EventServiceAccountCredentialRevoked, tenantID); n != 1 {
		t.Fatalf("ServiceAccountCredentialRevoked events = %d, want 1", n)
	}

	if resp, p := decodeErrorBody(t, http.MethodPost,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String()+"/credentials",
		sysHeaders(tenantID), issueOrRotateRequestBody{RotationID: newRotationID()}); resp.StatusCode != http.StatusUnprocessableEntity || p.ErrorCode != "PRINCIPAL_REVOKED" {
		t.Fatalf("TS-1 after TS-7: status %d errorCode %q, want 422 PRINCIPAL_REVOKED", resp.StatusCode, p.ErrorCode)
	}

	other := sysHeaders(newTenantID())
	if resp, p := decodeErrorBody(t, http.MethodPost, revokePrincipalPath(tenantID), other, nil); resp.StatusCode != http.StatusForbidden || p.ErrorCode != "TENANT_PATH_MISMATCH" {
		t.Fatalf("tenant mismatch: status %d errorCode %q", resp.StatusCode, p.ErrorCode)
	}
}
