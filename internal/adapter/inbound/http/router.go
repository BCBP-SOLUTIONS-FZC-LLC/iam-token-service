package http

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// Pinger is satisfied by any dependency /readyz must check.
type Pinger interface {
	Health(ctx context.Context) error
}

// DocsConfig controls whether the Swagger (OpenAPI, §5.5) and AsyncAPI
// (§7.3) docs surfaces are exposed — always mounted in a development
// environment (local, dev, test), opt-in via Enabled everywhere else, and
// bearer-token-locked there when AuthToken is set (§12.1).
type DocsConfig struct {
	Environment string
	Enabled     bool
	AuthToken   string
}

func (d DocsConfig) active() bool {
	return !d.Restricted() || d.Enabled
}

// Restricted reports whether Environment is anything but a development
// environment (local, dev, test — the platform observability vocabulary):
// staging, prod, a stale "production", an unknown value, or none at all.
// Restricted environments get production docs gating (TS-D23): staging
// used to mount the docs unconditionally and ignore DOCS_AUTH_TOKEN, and
// staging carries real tenant ids and the same internal API shape. Unknown
// and empty values are restricted so a typo can never expose the docs.
// cmd/server's startup check uses it too (enabled without a token fails).
func (d DocsConfig) Restricted() bool {
	switch strings.ToLower(strings.TrimSpace(d.Environment)) {
	case "local", "dev", "test":
		return false
	}
	return true
}

// Handlers bundles the Gin handler methods NewRouter wires onto routes.
type Handlers struct {
	Principal  *PrincipalHandler
	Credential *CredentialHandler
	JWKS       *JWKSHandler
}

// RouterConfig bundles every dependency NewRouter needs.
type RouterConfig struct {
	GinConfig gincommon.Config
	Docs      DocsConfig

	Handlers Handlers

	Postgres Pinger
	OpenBao  Pinger
	Outbox   Pinger
}

// Router owns the Gin engine for this service.
type Router struct {
	engine *gin.Engine
}

// Handler returns the http.Handler to serve.
func (r *Router) Handler() http.Handler { return r.engine }

// NewRouter builds and wires every route this service exposes: the
// unauthenticated infra probes, the docs surface, the EXT-6 JWKS route
// (unauthenticated by header, outside the protected group — §5.6) and the
// mesh-only, header-authenticated /api/v1/internal/* credential-lifecycle
// API (TS-1..TS-6, §5.1).
func NewRouter(cfg RouterConfig) *Router {
	errorLogger = cfg.GinConfig.Logger

	r := gin.New()
	r.HandleMethodNotAllowed = true

	// 1 MB body cap — this service's request/response bodies are tiny
	// (UUIDs, small JSON payloads, one secret string); no legitimate
	// caller needs more.
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
		c.Next()
	})
	r.Use(gincommon.ObservabilityMiddlewares(cfg.GinConfig)...)

	registerInfraRoutes(r, cfg)
	registerDocsRoutes(r, cfg)
	registerJWKSRoutes(r, cfg)
	registerInternalRoutes(r, cfg)

	return &Router{engine: r}
}

// ── Infra routes (unauthenticated, registered before auth so LB probes
// with no headers still get 200) ────────────────────────────────────────

type healthHandlers struct {
	postgres Pinger
	openbao  Pinger
	outbox   Pinger
}

func registerInfraRoutes(r *gin.Engine, cfg RouterConfig) {
	h := &healthHandlers{postgres: cfg.Postgres, openbao: cfg.OpenBao, outbox: cfg.Outbox}
	// Liveness goes through platform-gincommon (same handler as iam-user-profile).
	r.GET("/healthz", gincommon.HealthHandler())
	r.GET("/readyz", h.readyz)
	// /metrics is served on its own dedicated port (METRICS_PORT, §12) —
	// not on this router — so scraping never shares a listener with the
	// mesh-internal credential-lifecycle API.
}

// readyz checks Postgres and OpenBao reachability (§14.4 smoke). The checks
// run concurrently — sequentially, a single slow-but-healthy dependency
// could consume the whole readinessProbe timeout budget before the other
// even started, flapping readiness for reasons unrelated to it.
func (h *healthHandlers) readyz(c *gin.Context) {
	ctx := c.Request.Context()

	pingers := []struct {
		name string
		p    Pinger
	}{
		{"database", h.postgres},
		{"openbao", h.openbao},
		{"outbox", h.outbox},
	}

	type result struct {
		name string
		ok   bool
	}
	results := make(chan result, len(pingers))
	pending := 0
	for _, pc := range pingers {
		if pc.p == nil {
			continue
		}
		pending++
		go func(name string, p Pinger) {
			results <- result{name: name, ok: p.Health(ctx) == nil}
		}(pc.name, pc.p)
	}

	healthy := true
	checks := gin.H{}
	for i := 0; i < pending; i++ {
		r := <-results
		if r.ok {
			checks[r.name] = "ok"
		} else {
			checks[r.name] = "down"
			healthy = false
		}
	}

	status := http.StatusOK
	overall := "ready"
	if !healthy {
		status = http.StatusServiceUnavailable
		overall = "not ready"
	}
	c.JSON(status, gin.H{"status": overall, "checks": checks})
}

// ── Docs surface — Swagger (OpenAPI, §5.5) + AsyncAPI (§7.3/§12.1) ────────

func registerDocsRoutes(r *gin.Engine, cfg RouterConfig) {
	if !cfg.Docs.active() {
		return
	}

	secHeaders := func(c *gin.Context) {
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	}

	var authMiddleware gin.HandlerFunc = func(c *gin.Context) { c.Next() }
	if cfg.Docs.Restricted() && cfg.Docs.AuthToken != "" {
		expected := []byte("Bearer " + cfg.Docs.AuthToken)
		authMiddleware = func(c *gin.Context) {
			// Constant-time compare — a plain != leaks how many leading
			// bytes of a guessed token are correct via response latency.
			got := []byte(c.GetHeader("Authorization"))
			if subtle.ConstantTimeCompare(got, expected) != 1 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			c.Next()
		}
	}

	// docs/swagger/docs.go (generated by `make swag`) registers itself with
	// swaggo's global spec registry via its init() — ginSwagger.WrapHandler
	// reads from that registry, not from a file path. Importing
	// cmd/server's docs package (see cmd/server/main.go's blank import) is
	// what makes that registration happen for the running binary.
	stdSwagger := ginSwagger.WrapHandler(swaggerFiles.Handler)
	r.GET("/swagger/*any", secHeaders, authMiddleware, func(c *gin.Context) {
		switch {
		case strings.HasSuffix(c.Request.URL.Path, "/index.css"):
			SwaggerThemeHandler(c)
		case strings.HasSuffix(c.Request.URL.Path, "/swagger-initializer.js"):
			SwaggerInitializerHandler(c)
		default:
			stdSwagger(c)
		}
	})
	r.GET("/asyncapi", secHeaders, authMiddleware, AsyncAPIHandler)
	r.GET("/asyncapi.yaml", secHeaders, authMiddleware, AsyncAPIYAMLHandler)
}

// ── JWKS (EXT-6, §2.5) — unauthenticated by header, RLS-scoped from the
// path instead; registered before registerInternalRoutes' protected group
// so this one path never picks up ProtectedMiddlewares/GUCBridgeMiddleware ──

func registerJWKSRoutes(r *gin.Engine, cfg RouterConfig) {
	h := cfg.Handlers
	if h.JWKS == nil {
		return
	}
	r.GET("/api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json", h.JWKS.JWKS)
}

// ── Credential-lifecycle API — /api/v1/internal/* (TS-1..TS-6, §5.1/§25) ──

func registerInternalRoutes(r *gin.Engine, cfg RouterConfig) {
	h := cfg.Handlers

	// RequireIdentityHeaders first: it answers a missing/invalid identity
	// header with the frozen missing_identity_headers code before gincommon's
	// RequireAuth can answer it with a generic message.
	protected := []gin.HandlerFunc{RequireIdentityHeaders()}
	protected = append(protected, gincommon.ProtectedMiddlewares(cfg.GinConfig)...)
	protected = append(protected, GUCBridgeMiddleware(), RequireJSONContentType())
	// Every API route lives under /api/v1/internal (§5.1). The one route
	// there NOT in this group is the EXT-6 JWKS route (registerJWKSRoutes),
	// which carries no identity headers by design (§5.6).
	internal := r.Group("/api/v1/internal", protected...)

	tenantScoped := internal.Group("/tenants/:id", RequireTenantPathMatch())
	tenantScoped.POST("/service-accounts", h.Principal.Register)                                          // TS-4
	tenantScoped.GET("/service-accounts", h.Principal.FindBySub)                                          // TS-5 (AUTH-9)
	tenantScoped.GET("/service-accounts/platform-automation", h.Principal.ReadPlatformAutomation)         // TS-6 (TS-D17)
	tenantScoped.GET("/service-accounts/:principal_id", h.Principal.Read)                                 // TS-3
	tenantScoped.POST("/service-accounts/:principal_id/credentials", h.Credential.IssueOrRotate)          // TS-1
	tenantScoped.POST("/service-accounts/:principal_id/credentials/:version/revoke", h.Credential.Revoke) // TS-2
}
