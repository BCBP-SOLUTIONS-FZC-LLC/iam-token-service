package http

import (
	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// testGinConfig is a minimal gincommon.Config for tests that need the real
// ProtectedMiddlewares chain (RequireAuth + ContextMiddleware) ahead of
// GUCBridgeMiddleware, matching router.go's actual wiring order.
var testGinConfig = gincommon.Config{ServiceName: "iam-token-service-test", Domain: "iam", Environment: "test"}

// newTenantScopedTestRouter wires the real production middleware chain
// (ProtectedMiddlewares -> GUCBridgeMiddleware -> RequireTenantPathMatch)
// in front of a single registered route, mirroring registerInternalRoutes'
// tenantScoped group in router.go — used by the principal/credential
// handler test files so "missing identity headers"/"tenant path mismatch"
// behavior is exercised through the real chain, not reimplemented.
func newTenantScopedTestRouter(register func(g *gin.RouterGroup)) *gin.Engine {
	r := gin.New()
	r.Use(RequireIdentityHeaders())
	r.Use(gincommon.ProtectedMiddlewares(testGinConfig)...)
	r.Use(GUCBridgeMiddleware())
	g := r.Group("/tenants/:id", RequireTenantPathMatch())
	register(g)
	return r
}
