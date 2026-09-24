//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

type automationPrincipalResponseBody struct {
	PrincipalID      uuid.UUID `json:"principal_id"`
	TenantID         uuid.UUID `json:"tenant_id"`
	PrincipalSub     uuid.UUID `json:"principal_sub"`
	KeycloakClientID string    `json:"keycloak_client_id"`
	PrincipalType    string    `json:"principal_type"`
	Status           string    `json:"status"`
}

func readPlatformAutomation(t *testing.T, tenantID uuid.UUID) (*http.Response, automationPrincipalResponseBody) {
	t.Helper()
	var body automationPrincipalResponseBody
	resp := doJSON(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation",
		sysHeaders(tenantID), nil, &body)
	return resp, body
}

func registerWithSub(t *testing.T, tenantID uuid.UUID, sub uuid.UUID, clientID string) uuid.UUID {
	t.Helper()
	var body principalResponseBody
	resp := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID),
		registerRequestBody{PrincipalSub: sub.String(), KeycloakClientID: clientID},
		&body,
	)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d, want 200/201", resp.StatusCode)
	}
	return body.PrincipalID
}

// TestReadPlatformAutomation_ResolvesSubAcrossReMint covers TS-6 (TS-D17):
// the tenant's automation subject resolves from the tenant id alone, and an
// RP-3/RP-4 re-mint (TS-4 carry-over) changes principal_sub while
// principal_id stays put.
func TestReadPlatformAutomation_ResolvesSubAcrossReMint(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	if resp, _ := doRequest(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation",
		sysHeaders(tenantID), nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("before mint: status = %d, want 404", resp.StatusCode)
	}

	trialSub := uuid.New()
	principalID := registerWithSub(t, tenantID, trialSub, "platform-automation-"+tenantID.String())
	resp, body := readPlatformAutomation(t, tenantID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after mint: status = %d, want 200", resp.StatusCode)
	}
	if body.PrincipalSub != trialSub || body.PrincipalID != principalID || body.PrincipalType != "platform_automation" {
		t.Fatalf("after mint: got %+v, want sub %s principal %s", body, trialSub, principalID)
	}

	dedicatedSub := uuid.New()
	registerWithSub(t, tenantID, dedicatedSub, "platform-automation")
	resp, body = readPlatformAutomation(t, tenantID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after re-mint: status = %d, want 200", resp.StatusCode)
	}
	if body.PrincipalSub != dedicatedSub {
		t.Fatalf("after re-mint: principal_sub = %s, want %s", body.PrincipalSub, dedicatedSub)
	}
	if body.PrincipalID != principalID {
		t.Fatalf("after re-mint: principal_id = %s, want unchanged %s", body.PrincipalID, principalID)
	}
}
