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

func newPrincipalTestRouter(svc PrincipalService) *gin.Engine {
	h := NewPrincipalHandler(svc)
	return newTenantScopedTestRouter(func(g *gin.RouterGroup) {
		g.POST("/service-accounts", h.Register)
		g.GET("/service-accounts/:principal_id", h.Read)
	})
}

func TestPrincipalHandler_Register(t *testing.T) {
	tenantID := uuid.New()
	principalSub := uuid.New()

	t.Run("201 on first creation", func(t *testing.T) {
		svc := &fakePrincipalService{registerResult: &service.RegisterResult{
			PrincipalID: uuid.New(), TenantID: tenantID, PrincipalType: domain.PrincipalTypePlatformAutomation,
			Status: domain.PrincipalStatusActive, RecordVersion: 1, Created: true,
		}}
		r := newPrincipalTestRouter(svc)
		var body principalResponseBody
		rec := doJSON(t, r, http.MethodPost, "/tenants/"+tenantID.String()+"/service-accounts", sysHeaders(tenantID),
			registerRequestBody{PrincipalSub: principalSub.String(), KeycloakClientID: domain.KeycloakClientPlatformAutomation}, &body)
		require.Equal(t, http.StatusCreated, rec.Code)
		assert.Equal(t, tenantID, body.TenantID)
		assert.Equal(t, "active", body.Status)
	})

	t.Run("200 on idempotent repeat", func(t *testing.T) {
		svc := &fakePrincipalService{registerResult: &service.RegisterResult{
			PrincipalID: uuid.New(), TenantID: tenantID, Status: domain.PrincipalStatusActive,
			RecordVersion: 1, Created: false,
		}}
		r := newPrincipalTestRouter(svc)
		rec := doJSON(t, r, http.MethodPost, "/tenants/"+tenantID.String()+"/service-accounts", sysHeaders(tenantID),
			registerRequestBody{PrincipalSub: principalSub.String(), KeycloakClientID: domain.KeycloakClientPlatformAutomation}, nil)
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("401 missing identity headers", func(t *testing.T) {
		r := newPrincipalTestRouter(&fakePrincipalService{})
		rec := doRequest(t, r, http.MethodPost, "/tenants/"+tenantID.String()+"/service-accounts", map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("400 malformed JSON body", func(t *testing.T) {
		r := newPrincipalTestRouter(&fakePrincipalService{})
		rec := doRequest(t, r, http.MethodPost, "/tenants/"+tenantID.String()+"/service-accounts", sysHeaders(tenantID), []byte(`{not-json`))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "invalid_request", er.Error)
	})

	t.Run("400 non-UUID principal_sub", func(t *testing.T) {
		r := newPrincipalTestRouter(&fakePrincipalService{})
		rec := doJSON(t, r, http.MethodPost, "/tenants/"+tenantID.String()+"/service-accounts", sysHeaders(tenantID),
			registerRequestBody{PrincipalSub: "not-a-uuid", KeycloakClientID: domain.KeycloakClientPlatformAutomation}, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "principal_sub", er.Details["field"])
	})

	t.Run("403 tenant path mismatch", func(t *testing.T) {
		r := newPrincipalTestRouter(&fakePrincipalService{})
		otherTenant := uuid.New()
		rec := doJSON(t, r, http.MethodPost, "/tenants/"+otherTenant.String()+"/service-accounts", sysHeaders(tenantID),
			registerRequestBody{PrincipalSub: principalSub.String(), KeycloakClientID: domain.KeycloakClientPlatformAutomation}, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("maps every domain error from the service", func(t *testing.T) {
		cases := []struct {
			name       string
			err        error
			wantStatus int
		}{
			{"invalid_request", domain.NewError(domain.ErrInvalidRequest, "bad").WithDetails(map[string]any{"field": "keycloak_client_id"}), http.StatusBadRequest},
			{"db_unavailable", domain.NewError(domain.ErrDBUnavailable, "down"), http.StatusServiceUnavailable},
			{"unclassified", assert.AnError, http.StatusInternalServerError},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				svc := &fakePrincipalService{registerErr: tc.err}
				r := newPrincipalTestRouter(svc)
				rec := doJSON(t, r, http.MethodPost, "/tenants/"+tenantID.String()+"/service-accounts", sysHeaders(tenantID),
					registerRequestBody{PrincipalSub: principalSub.String(), KeycloakClientID: domain.KeycloakClientPlatformAutomation}, nil)
				assert.Equal(t, tc.wantStatus, rec.Code)
			})
		}
	})
}

func TestPrincipalHandler_Read(t *testing.T) {
	tenantID := uuid.New()
	principalID := uuid.New()
	issuedAt := time.Now().UTC()
	expiresAt := issuedAt.Add(5 * time.Minute)

	t.Run("200 with credentials, one with a non-nil expires_at", func(t *testing.T) {
		svc := &fakePrincipalService{readResult: &service.ReadPrincipalResult{
			PrincipalID: principalID, TenantID: tenantID, KeycloakClientID: domain.KeycloakClientPlatformAutomation,
			PrincipalType: domain.PrincipalTypePlatformAutomation, Status: domain.PrincipalStatusActive, RecordVersion: 2,
			Credentials: []service.CredentialSummary{
				{Version: 2, Status: domain.CredentialStatusActive, OpenBaoPath: "iam/serviceaccount/x/y/v2", IssuedAt: issuedAt},
				{Version: 1, Status: domain.CredentialStatusRotating, OpenBaoPath: "iam/serviceaccount/x/y/v1", IssuedAt: issuedAt, ExpiresAt: &expiresAt},
			},
		}}
		r := newPrincipalTestRouter(svc)
		var body readPrincipalResponseBody
		rec := doJSON(t, r, http.MethodGet, "/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String(), sysHeaders(tenantID), nil, &body)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Len(t, body.Credentials, 2)
		assert.Nil(t, body.Credentials[0].ExpiresAt)
		require.NotNil(t, body.Credentials[1].ExpiresAt)
		assert.Equal(t, expiresAt.Format(time.RFC3339), *body.Credentials[1].ExpiresAt)
	})

	t.Run("401 missing identity headers", func(t *testing.T) {
		r := newPrincipalTestRouter(&fakePrincipalService{})
		rec := doRequest(t, r, http.MethodGet, "/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String(), nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("400 non-UUID principal_id", func(t *testing.T) {
		r := newPrincipalTestRouter(&fakePrincipalService{})
		rec := doRequest(t, r, http.MethodGet, "/tenants/"+tenantID.String()+"/service-accounts/not-a-uuid", sysHeaders(tenantID), nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "principal_id", er.Details["field"])
	})

	t.Run("404 principal not found", func(t *testing.T) {
		svc := &fakePrincipalService{readErr: domain.NewError(domain.ErrPrincipalNotFound, "no principal")}
		r := newPrincipalTestRouter(svc)
		rec := doRequest(t, r, http.MethodGet, "/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String(), sysHeaders(tenantID), nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

// TestPrincipalHandler_DefensiveMissingRequestContext exercises the
// handler-level `requestctx.FromContext` !ok branch directly — unreachable
// in the real chain (GUCBridgeMiddleware always sets it before next()),
// but defensive, so covered here by calling the handler with no
// requestctx-setting middleware at all.
func TestPrincipalHandler_DefensiveMissingRequestContext(t *testing.T) {
	h := NewPrincipalHandler(&fakePrincipalService{})
	r := gin.New()
	r.POST("/service-accounts", h.Register)
	r.GET("/service-accounts/:principal_id", h.Read)

	rec := doRequest(t, r, http.MethodPost, "/service-accounts", map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = doRequest(t, r, http.MethodGet, "/service-accounts/"+uuid.New().String(), nil, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
