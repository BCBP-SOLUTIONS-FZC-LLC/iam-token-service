package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

func newFullTestRouter(t *testing.T, docs DocsConfig, principalSvc PrincipalService, credentialSvc CredentialService, postgres, openbao, outbox Pinger) *Router {
	t.Helper()
	return NewRouter(RouterConfig{
		GinConfig: testGinConfig,
		Docs:      docs,
		Handlers: Handlers{
			Principal:  NewPrincipalHandler(principalSvc),
			Credential: NewCredentialHandler(credentialSvc),
		},
		Postgres: postgres,
		OpenBao:  openbao,
		Outbox:   outbox,
	})
}

func TestRouter_Healthz(t *testing.T) {
	r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
	rec := doRequest(t, r.Handler(), http.MethodGet, "/healthz", nil, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRouter_Readyz(t *testing.T) {
	t.Run("all healthy — 200", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/readyz", nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "ready", body.Status)
		assert.Equal(t, "ok", body.Checks["database"])
		assert.Equal(t, "ok", body.Checks["openbao"])
		assert.Equal(t, "ok", body.Checks["outbox"])
	})

	t.Run("one dependency down — 503", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{healthErr: assert.AnError}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/readyz", nil, nil)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		var body struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "not ready", body.Status)
		assert.Equal(t, "down", body.Checks["database"])
		assert.Equal(t, "ok", body.Checks["openbao"])
	})

	t.Run("nil pinger is skipped, not counted", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{}, &fakePrincipalService{}, &fakeCredentialService{}, nil, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/readyz", nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Checks map[string]string `json:"checks"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		_, present := body.Checks["database"]
		assert.False(t, present)
	})
}

func TestRouter_DocsSurface_Gating(t *testing.T) {
	t.Run("production, not enabled — docs routes absent", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "production", Enabled: false}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", nil, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("prod (APP_ENV vocabulary), not enabled — docs routes absent", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "prod", Enabled: false}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", nil, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("prod + AuthToken — requires bearer token", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "prod", Enabled: true, AuthToken: "s3cr3t"}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		rec = doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", map[string]string{"Authorization": "Bearer s3cr3t"}, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("non-production — active without Enabled", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "dev"}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", nil, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("production, Enabled — active, no auth token required when unset", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "production", Enabled: true}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", nil, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("production + AuthToken — requires bearer token", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "production", Enabled: true, AuthToken: "s3cr3t"}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})

		rec := doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		rec = doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", map[string]string{"Authorization": "Bearer wrong"}, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		// A short/garbage header must not panic subtle.ConstantTimeCompare.
		rec = doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", map[string]string{"Authorization": "x"}, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)

		rec = doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", map[string]string{"Authorization": "Bearer s3cr3t"}, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("staging, not enabled — docs routes absent (TS-D23)", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "staging"}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", nil, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("staging + AuthToken — requires bearer token (TS-D23)", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "staging", Enabled: true, AuthToken: "s3cr3t"}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
		rec := doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		rec = doRequest(t, r.Handler(), http.MethodGet, "/swagger/index.html", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		rec = doRequest(t, r.Handler(), http.MethodGet, "/asyncapi.yaml", map[string]string{"Authorization": "Bearer s3cr3t"}, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("unknown or empty environment is restricted", func(t *testing.T) {
		for _, env := range []string{"", "qa", "Staging "} {
			assert.True(t, DocsConfig{Environment: env}.Restricted(), "%q", env)
		}
		for _, env := range []string{"local", "dev", "TEST"} {
			assert.False(t, DocsConfig{Environment: env}.Restricted(), "%q", env)
		}
	})

	t.Run("swagger routes reachable via all three switch branches without panicking", func(t *testing.T) {
		r := newFullTestRouter(t, DocsConfig{Environment: "dev"}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})

		assert.NotPanics(t, func() {
			doRequest(t, r.Handler(), http.MethodGet, "/swagger/index.css", nil, nil)
		})
		assert.NotPanics(t, func() {
			doRequest(t, r.Handler(), http.MethodGet, "/swagger/swagger-initializer.js", nil, nil)
		})
		assert.NotPanics(t, func() {
			doRequest(t, r.Handler(), http.MethodGet, "/swagger/index.html", nil, nil)
		})
	})
}

// TestRouter_JWKSRoute_RegisteredWhenHandlerPresent covers
// registerJWKSRoutes' actual registration branch — newFullTestRouter's
// Handlers.JWKS is otherwise always nil, so every other router test only
// exercises the early-return.
func TestRouter_JWKSRoute_RegisteredWhenHandlerPresent(t *testing.T) {
	tenantID := uuid.New()
	r := NewRouter(RouterConfig{
		GinConfig: testGinConfig,
		Handlers: Handlers{
			Principal:  NewPrincipalHandler(&fakePrincipalService{}),
			Credential: NewCredentialHandler(&fakeCredentialService{}),
			JWKS:       NewJWKSHandler(&fakeJWKSService{keys: []map[string]any{{"kty": "RSA", "kid": "k1"}}}),
		},
		Postgres: fakePinger{}, OpenBao: fakePinger{}, Outbox: fakePinger{},
	})
	rec := doRequest(t, r.Handler(), http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation/jwks.json", nil, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRouter_InternalRoutes_Smoke(t *testing.T) {
	tenantID := uuid.New()
	principalID := uuid.New()

	principalSvc := &fakePrincipalService{
		registerResult:   &service.RegisterResult{PrincipalID: principalID, TenantID: tenantID, Status: domain.PrincipalStatusActive, Created: true},
		readResult:       &service.ReadPrincipalResult{PrincipalID: principalID, TenantID: tenantID, Status: domain.PrincipalStatusActive},
		automationResult: &service.AutomationPrincipalResult{PrincipalID: principalID, TenantID: tenantID, Status: domain.PrincipalStatusActive},
	}
	credentialSvc := &fakeCredentialService{
		issueResult:  &service.IssueOrRotateResult{Version: 1, Secret: "s"},
		revokeResult: &service.RevokeResult{Version: 1, Status: domain.CredentialStatusRevoked},
	}
	r := newFullTestRouter(t, DocsConfig{}, principalSvc, credentialSvc, fakePinger{}, fakePinger{}, fakePinger{})
	headers := sysHeaders(tenantID)

	cases := []struct {
		name   string
		method string
		path   string
		body   []byte
		want   int
	}{
		{"TS-4 register", http.MethodPost, "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts", []byte(`{"principal_sub":"` + uuid.New().String() + `","keycloak_client_id":"platform-automation"}`), http.StatusCreated},
		{"TS-6 read platform-automation", http.MethodGet, "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/platform-automation", nil, http.StatusOK},
		{"TS-3 read", http.MethodGet, "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/" + principalID.String(), nil, http.StatusOK},
		{"TS-1 issue", http.MethodPost, "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/" + principalID.String() + "/credentials", []byte(`{"rotation_id":"` + uuid.New().String() + `"}`), http.StatusCreated},
		{"TS-2 revoke", http.MethodPost, "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/" + principalID.String() + "/credentials/1/revoke", nil, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, r.Handler(), tc.method, tc.path, headers, tc.body)
			assert.Equal(t, tc.want, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// Every way an identity header can be missing or unusable is answered with
// the frozen §17 code — never gincommon RequireAuth's generic body.
func TestRouter_MissingIdentityHeaders_FrozenCode(t *testing.T) {
	r := newFullTestRouter(t, DocsConfig{Environment: "dev"}, &fakePrincipalService{}, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
	tenantID := uuid.New()
	path := "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/platform-automation"

	cases := map[string]map[string]string{
		"no headers":         nil,
		"tenant header only": {"x-tenant-id": tenantID.String()},
		"user header only":   {"x-user-id": domain.SystemPrincipalID.String()},
		"blank user header":  {"x-user-id": "  ", "x-tenant-id": tenantID.String()},
		"non-UUID tenant":    {"x-user-id": domain.SystemPrincipalID.String(), "x-tenant-id": "acme"},
		"non-system user":    {"x-user-id": uuid.NewString(), "x-tenant-id": tenantID.String()},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			rec := doRequest(t, r.Handler(), http.MethodGet, path, headers, nil)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			er := decodeErrorBody(t, rec)
			assert.Equal(t, "missing_identity_headers", er.Error)
			assert.Nil(t, er.Details, "§17: no details on missing_identity_headers")
		})
	}
}

// x-tenant-roles is unused here; garbage in it must not turn a valid request
// into gincommon's generic 401.
func TestRouter_GarbageTenantRolesHeaderIsIgnored(t *testing.T) {
	tenantID, principalID := uuid.New(), uuid.New()
	principalSvc := &fakePrincipalService{
		automationResult: &service.AutomationPrincipalResult{PrincipalID: principalID, TenantID: tenantID, Status: domain.PrincipalStatusActive},
	}
	r := newFullTestRouter(t, DocsConfig{Environment: "dev"}, principalSvc, &fakeCredentialService{}, fakePinger{}, fakePinger{}, fakePinger{})
	headers := sysHeaders(tenantID)
	headers["x-tenant-roles"] = strings.Repeat("\x00not,a;valid role!", 2000)

	rec := doRequest(t, r.Handler(), http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation", headers, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

func TestRequireIdentityHeaders_RepeatedHeaderIsMissingIdentity(t *testing.T) {
	r := gin.New()
	r.GET("/probe", RequireIdentityHeaders(), func(c *gin.Context) { c.Status(http.StatusNoContent) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Add("x-user-id", domain.SystemPrincipalID.String())
	req.Header.Add("x-tenant-id", uuid.NewString())
	req.Header.Add("x-tenant-id", uuid.NewString())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "missing_identity_headers", decodeErrorBody(t, rec).Error)

	req = httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("x-user-id", domain.SystemPrincipalID.String())
	req.Header.Set("x-tenant-id", uuid.NewString())
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}
