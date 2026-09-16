package http

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// newTestEngine builds a bare Gin engine with a single terminal 200 handler
// wired behind mw, for isolated middleware tests.
func newTestEngine(mw ...gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	chain := append(append([]gin.HandlerFunc{}, mw...), func(c *gin.Context) { c.Status(http.StatusOK) })
	r.Any("/probe", chain...)
	return r
}

func TestGUCBridgeMiddleware(t *testing.T) {
	tenantID := uuid.New()

	t.Run("no platform request context — 401", func(t *testing.T) {
		r := newTestEngine(GUCBridgeMiddleware())
		rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "missing_identity_headers", er.Error)
	})

	t.Run("missing headers rejected upstream by RequireAuth", func(t *testing.T) {
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-UUID x-user-id — 401", func(t *testing.T) {
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(t, r, http.MethodGet, "/probe", map[string]string{"x-user-id": "not-a-uuid", "x-tenant-id": tenantID.String()}, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("x-user-id present but not the reserved system principal — 401", func(t *testing.T) {
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(t, r, http.MethodGet, "/probe", map[string]string{"x-user-id": uuid.New().String(), "x-tenant-id": tenantID.String()}, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-UUID x-tenant-id — 401", func(t *testing.T) {
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(t, r, http.MethodGet, "/probe", map[string]string{"x-user-id": domain.SystemPrincipalID.String(), "x-tenant-id": "not-a-uuid"}, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("valid identity populates requestctx and the pgcommon GUC", func(t *testing.T) {
		var gotRC *requestctx.RequestContext
		var gotGUC pgcommonGUCSetLike
		r := gin.New()
		r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
		r.Use(GUCBridgeMiddleware())
		r.GET("/probe", func(c *gin.Context) {
			rc, _ := requestctx.FromContext(c.Request.Context())
			gotRC = rc
			g, _ := pgcommon.GUCSetFromContext(c.Request.Context())
			gotGUC = pgcommonGUCSetLike{TenantID: g.TenantID, UserID: g.UserID}
			c.Status(http.StatusOK)
		})
		rec := doRequest(t, r, http.MethodGet, "/probe", sysHeaders(tenantID), nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.NotNil(t, gotRC)
		assert.Equal(t, tenantID, gotRC.TenantID)
		assert.Equal(t, domain.SystemPrincipalID, gotRC.UserID)
		assert.Equal(t, tenantID.String(), gotGUC.TenantID)
		assert.Equal(t, domain.SystemPrincipalID.String(), gotGUC.UserID)
	})
}

// pgcommonGUCSetLike avoids importing pgcommon's GUCSet type name directly
// in the struct literal above (keeps the assertion focused on the two
// fields GUCBridgeMiddleware actually sets).
type pgcommonGUCSetLike struct{ TenantID, UserID string }

func TestRequireTenantPathMatch(t *testing.T) {
	tenantID := uuid.New()

	t.Run("no requestctx present — fails closed with 401", func(t *testing.T) {
		r := newTestEngine(RequireTenantPathMatch())
		rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-UUID :id — 400", func(t *testing.T) {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			ctx := requestctx.WithContext(c.Request.Context(), &requestctx.RequestContext{TenantID: tenantID})
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
		r.GET("/tenants/:id/probe", RequireTenantPathMatch(), func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(t, r, http.MethodGet, "/tenants/not-a-uuid/probe", nil, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "id", er.Details["field"])
	})

	t.Run("mismatch — 403 tenant_path_mismatch", func(t *testing.T) {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			ctx := requestctx.WithContext(c.Request.Context(), &requestctx.RequestContext{TenantID: tenantID})
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
		r.GET("/tenants/:id/probe", RequireTenantPathMatch(), func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(t, r, http.MethodGet, "/tenants/"+uuid.New().String()+"/probe", nil, nil)
		require.Equal(t, http.StatusForbidden, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "tenant_path_mismatch", er.Error)
	})

	t.Run("match — next runs", func(t *testing.T) {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			ctx := requestctx.WithContext(c.Request.Context(), &requestctx.RequestContext{TenantID: tenantID})
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
		r.GET("/tenants/:id/probe", RequireTenantPathMatch(), func(c *gin.Context) { c.Status(http.StatusOK) })
		rec := doRequest(t, r, http.MethodGet, "/tenants/"+tenantID.String()+"/probe", nil, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestRequireJSONContentType(t *testing.T) {
	t.Run("GET always passes regardless of content-type", func(t *testing.T) {
		r := newTestEngine(RequireJSONContentType())
		rec := doRequest(t, r, http.MethodGet, "/probe", map[string]string{"Content-Type": "text/plain"}, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("POST with zero content-length passes", func(t *testing.T) {
		r := newTestEngine(RequireJSONContentType())
		rec := doRequest(t, r, http.MethodPost, "/probe", nil, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("POST with application/json passes", func(t *testing.T) {
		r := newTestEngine(RequireJSONContentType())
		rec := doRequest(t, r, http.MethodPost, "/probe", map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("POST with wrong content-type — 415", func(t *testing.T) {
		r := newTestEngine(RequireJSONContentType())
		rec := doRequest(t, r, http.MethodPost, "/probe", map[string]string{"Content-Type": "text/plain"}, []byte(`hi`))
		require.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	})

	t.Run("PUT/PATCH also gated", func(t *testing.T) {
		r := newTestEngine(RequireJSONContentType())
		rec := doRequest(t, r, http.MethodPut, "/probe", map[string]string{"Content-Type": "text/plain"}, []byte(`hi`))
		assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
		rec = doRequest(t, r, http.MethodPatch, "/probe", map[string]string{"Content-Type": "text/plain"}, []byte(`hi`))
		assert.Equal(t, http.StatusUnsupportedMediaType, rec.Code)
	})
}
