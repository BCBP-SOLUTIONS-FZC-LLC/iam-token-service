package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/pkg/requestctx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// writeMissingIdentityHeaders writes the frozen §17 missing_identity_headers
// 401 — no `details` field per the taxonomy.
func writeMissingIdentityHeaders(c *gin.Context) {
	writeError(c, string(domain.ErrMissingIdentityHeaders), http.StatusUnauthorized, nil)
}

// GUCBridgeMiddleware runs after gincommon.ProtectedMiddlewares (which
// validates x-user-id/x-tenant-id are merely present). It additionally
// enforces §5.2's single authorization rule for this service — the ONLY
// accepted x-user-id on any /api/v1/internal/* route is the reserved
// iam-system principal — parses x-tenant-id as a UUID, stores a
// requestctx.RequestContext for handlers, and writes the pgcommon GUCSet so
// every checked-out connection binds `SET LOCAL app.tenant_id`/`app.user_id`
// inside the transaction (RLS-6). A session-scoped SET would leak across
// pooled backends and defeat RLS.
func GUCBridgeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		platformRc, ok := gincommon.RequestContext(c)
		if !ok {
			writeMissingIdentityHeaders(c)
			return
		}
		userID, err := uuid.Parse(platformRc.UserID)
		if err != nil || userID != domain.SystemPrincipalID {
			writeMissingIdentityHeaders(c)
			return
		}
		tenantID, err := uuid.Parse(platformRc.TenantID)
		if err != nil {
			writeMissingIdentityHeaders(c)
			return
		}

		rc := &requestctx.RequestContext{UserID: userID, TenantID: tenantID}
		ctx := requestctx.WithContext(c.Request.Context(), rc)

		g, _ := pgcommon.GUCSetFromContext(ctx)
		g.UserID = userID.String()
		g.TenantID = tenantID.String()
		ctx = pgcommon.WithGUCSet(ctx, g)

		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// RequireTenantPathMatch enforces §5.1: the path `:id` segment is the
// target tenant, validated equal to the x-tenant-id GUC before any DB
// checkout — a mismatch is 403 (§5.5). Registered after GUCBridgeMiddleware
// on every route whose path carries `:id`.
//
// §17's frozen error taxonomy has no named code for this 403 (only §5.5's
// status table documents the case) — `tenant_path_mismatch` is this
// service's own choice for the `error` field, not a frozen name.
func RequireTenantPathMatch() gin.HandlerFunc {
	return func(c *gin.Context) {
		rc, ok := requestctx.FromContext(c.Request.Context())
		if !ok {
			// Unreachable in production — GUCBridgeMiddleware always runs
			// first and aborts on failure — but fail closed regardless.
			writeMissingIdentityHeaders(c)
			return
		}
		pathTenantID, err := uuid.Parse(c.Param("id"))
		if err != nil {
			writeInvalidRequest(c, "id", "path tenant id must be a UUID")
			return
		}
		if pathTenantID != rc.TenantID {
			writeError(c, "tenant_path_mismatch", http.StatusForbidden, nil)
			return
		}
		c.Next()
	}
}

// RequireJSONContentType rejects POST/PUT/PATCH requests carrying a body
// without application/json (§5.1).
func RequireJSONContentType() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
		default:
			c.Next()
			return
		}
		if c.Request.ContentLength == 0 {
			c.Next()
			return
		}
		if c.ContentType() != "application/json" {
			writeError(c, "unsupported_media_type", http.StatusUnsupportedMediaType, nil)
			return
		}
		c.Next()
	}
}
