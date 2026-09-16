//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// registerPrincipal registers a fresh platform-automation principal for
// tenantID and returns its id, failing the test on any non-2xx response.
func registerPrincipal(t *testing.T, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var body principalResponseBody
	resp := doJSON(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID),
		registerRequestBody{PrincipalSub: uuid.NewString(), KeycloakClientID: "platform-automation"},
		&body,
	)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("registerPrincipal: status = %d, want 201", resp.StatusCode)
	}
	return body.PrincipalID
}

type issueOrRotateRequestBody struct {
	RotationID     string `json:"rotation_id"`
	OverlapSeconds *int   `json:"overlap_seconds,omitempty"`
}

type issueOrRotateResponseBody struct {
	Version        int     `json:"version"`
	Secret         string  `json:"secret"`
	OpenBaoPath    string  `json:"openbao_path"`
	ExpiresPriorAt *string `json:"expires_prior_at,omitempty"`
	RecordVersion  int     `json:"record_version"`
}

// issueOrRotate calls TS-1 for (tenantID, principalID) with a fresh
// rotation_id and returns the decoded response alongside the raw HTTP
// response, so callers can assert on status codes themselves.
func issueOrRotate(t *testing.T, tenantID, principalID uuid.UUID) (*http.Response, issueOrRotateResponseBody) {
	t.Helper()
	var body issueOrRotateResponseBody
	resp := doJSON(t, http.MethodPost,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String()+"/credentials",
		sysHeaders(tenantID), issueOrRotateRequestBody{RotationID: newRotationID()}, &body)
	return resp, body
}

type revokeResponseBody struct {
	Version              int    `json:"version"`
	Status               string `json:"status"`
	RevokedAt            string `json:"revoked_at"`
	KeycloakInvalidation string `json:"keycloak_invalidation"`
}
