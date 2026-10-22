package http

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// TS-7 — POST …/service-accounts/platform-automation/revoke.

func ts7Path(tenantID uuid.UUID) string {
	return "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/platform-automation/revoke"
}

func revokedPrincipal(tenantID uuid.UUID) *service.RevokePrincipalResult {
	return &service.RevokePrincipalResult{
		PrincipalID: uuid.New(), TenantID: tenantID, KeycloakClientID: "platform-automation-" + tenantID.String(),
		Status: domain.PrincipalStatusRevoked, RecordVersion: 2,
		RevokedAt: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC), Versions: []int{1, 2}, NewlyRevoked: 2,
	}
}

func TestCredentialHandler_RevokePrincipal(t *testing.T) {
	tenantID := uuid.New()

	t.Run("200 with the revoked principal, through the full router", func(t *testing.T) {
		want := revokedPrincipal(tenantID)
		svc := &fakeCredentialService{revokePrincipalResult: want}
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, svc, fakePinger{}, fakePinger{}, fakePinger{})
		var body revokePrincipalResponseBody
		rec := doJSON(t, r.Handler(), http.MethodPost, ts7Path(tenantID), sysHeaders(tenantID), nil, &body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, revokePrincipalResponseBody{
			PrincipalID: want.PrincipalID, TenantID: tenantID, KeycloakClientID: want.KeycloakClientID,
			Status: "revoked", RecordVersion: 2, RevokedAt: "2026-10-22T09:00:00Z",
			RevokedVersions: []int{1, 2}, KeycloakInvalidation: "caller_responsibility",
		}, body)
	})

	t.Run("no Content-Type is fine without a body", func(t *testing.T) {
		svc := &fakeCredentialService{revokePrincipalResult: revokedPrincipal(tenantID)}
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, svc, fakePinger{}, fakePinger{}, fakePinger{})
		headers := sysHeaders(tenantID)
		delete(headers, "Content-Type")
		rec := doRequest(t, r.Handler(), http.MethodPost, ts7Path(tenantID), headers, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("403 TENANT_PATH_MISMATCH never reaches the service", func(t *testing.T) {
		svc := &fakeCredentialService{revokePrincipalResult: revokedPrincipal(tenantID)}
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, svc, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodPost, ts7Path(uuid.New()), sysHeaders(tenantID), nil)
		require.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, "TENANT_PATH_MISMATCH", decodeErrorBody(t, rec).ErrorCode)
		assert.Zero(t, svc.revokePrincipalCalls)
	})

	t.Run("401 MISSING_IDENTITY_HEADERS for any x-user-id but iam-system", func(t *testing.T) {
		svc := &fakeCredentialService{revokePrincipalResult: revokedPrincipal(tenantID)}
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, svc, fakePinger{}, fakePinger{}, fakePinger{})
		for name, userID := range map[string]string{"other user": uuid.NewString(), "missing": ""} {
			t.Run(name, func(t *testing.T) {
				headers := sysHeaders(tenantID)
				if userID == "" {
					delete(headers, "x-user-id")
				} else {
					headers["x-user-id"] = userID
				}
				rec := doRequest(t, r.Handler(), http.MethodPost, ts7Path(tenantID), headers, nil)
				require.Equal(t, http.StatusUnauthorized, rec.Code)
				assert.Equal(t, "MISSING_IDENTITY_HEADERS", decodeErrorBody(t, rec).ErrorCode)
			})
		}
		assert.Zero(t, svc.revokePrincipalCalls)
	})

	t.Run("415 when a non-JSON body is sent", func(t *testing.T) {
		svc := &fakeCredentialService{revokePrincipalResult: revokedPrincipal(tenantID)}
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, svc, fakePinger{}, fakePinger{}, fakePinger{})
		headers := sysHeaders(tenantID)
		headers["Content-Type"] = "text/plain"
		rec := doRequest(t, r.Handler(), http.MethodPost, ts7Path(tenantID), headers, []byte("x"))
		assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	})

	t.Run("GET is not TS-7", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, ts7Path(tenantID), sysHeaders(tenantID), nil)
		assert.NotEqual(t, http.StatusOK, rec.Code)
	})

	t.Run("maps every domain error from the service", func(t *testing.T) {
		cases := []struct {
			name       string
			err        error
			wantStatus int
			wantCode   string
		}{
			{"not found", domain.NewError(domain.ErrPrincipalNotFound, "x"), http.StatusNotFound, "PRINCIPAL_NOT_FOUND"},
			{"rotation in flight", domain.NewError(domain.ErrRotationInFlight, "x").WithDetails(map[string]any{"active_rotation_id": uuid.NewString()}), http.StatusConflict, "ROTATION_IN_FLIGHT"},
			{"optimistic lock", domain.NewError(domain.ErrOptimisticLockConflict, "x"), http.StatusConflict, "OPTIMISTIC_LOCK_CONFLICT"},
			{"openbao down", domain.NewError(domain.ErrSecretStoreUnavailable, "x"), http.StatusBadGateway, "SECRET_STORE_UNAVAILABLE"},
			{"db down", domain.NewError(domain.ErrDBUnavailable, "x"), http.StatusServiceUnavailable, "DB_UNAVAILABLE"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				svc := &fakeCredentialService{revokePrincipalErr: tc.err}
				r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, svc, fakePinger{}, fakePinger{}, fakePinger{})
				rec := doRequest(t, r.Handler(), http.MethodPost, ts7Path(tenantID), sysHeaders(tenantID), nil)
				require.Equal(t, tc.wantStatus, rec.Code)
				assert.Equal(t, tc.wantCode, decodeErrorBody(t, rec).ErrorCode)
			})
		}
	})

	t.Run("defensive: no request context is 401", func(t *testing.T) {
		h := NewCredentialHandler(&fakeCredentialService{})
		r := gin.New()
		r.POST("/revoke", h.RevokePrincipal)
		rec := doRequest(t, r, http.MethodPost, "/revoke", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

// The static platform-automation segment must not shadow the
// :principal_id routes beside it, and TS-6 / TS-1 / TS-2 still route.
func TestRouter_TS7DoesNotShadowSiblingRoutes(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	principalSvc := &fakePrincipalService{
		automationResult: &service.AutomationPrincipalResult{PrincipalID: principalID, TenantID: tenantID, Status: domain.PrincipalStatusActive},
	}
	credentialSvc := &fakeCredentialService{
		issueResult:           &service.IssueOrRotateResult{Version: 1, Secret: "s"},
		revokeResult:          &service.RevokeResult{Version: 1, Status: domain.CredentialStatusRevoked},
		revokePrincipalResult: revokedPrincipal(tenantID),
	}
	r := newFullTestRouter(t, DocsConfig{}, principalSvc, credentialSvc, fakePinger{}, fakePinger{}, fakePinger{})
	base := "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/"
	h := sysHeaders(tenantID)

	assert.Equal(t, http.StatusOK, doRequest(t, r.Handler(), http.MethodGet, base+"platform-automation", h, nil).Code, "TS-6")
	assert.Equal(t, http.StatusCreated, doRequest(t, r.Handler(), http.MethodPost, base+principalID.String()+"/credentials", h, []byte(`{"rotation_id":"`+uuid.NewString()+`"}`)).Code, "TS-1")
	assert.Equal(t, http.StatusOK, doRequest(t, r.Handler(), http.MethodPost, base+principalID.String()+"/credentials/1/revoke", h, nil).Code, "TS-2")
	assert.Equal(t, http.StatusOK, doRequest(t, r.Handler(), http.MethodPost, base+"platform-automation/revoke", h, nil).Code, "TS-7")
	assert.Equal(t, 1, credentialSvc.revokePrincipalCalls)
}
