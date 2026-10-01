package http

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// JWKSService is the subset of *service.JWKSService this handler calls —
// mirrors PrincipalHandler/CredentialHandler's interface-not-concrete-type
// pattern for unit-testability against a hand-written fake. skipped is the
// number of live credentials whose key could not be read/parsed
// (production-readiness review, TS-D15) — see JWKSService's own doc
// comment (internal/core/service/jwks_service.go) for why this matters.
type JWKSService interface {
	PublicKeys(ctx context.Context, tenantID uuid.UUID) (keys []map[string]any, skipped int, err error)
}

// JWKSHandler implements EXT-6's JWKS-serving route (§2.5).
type JWKSHandler struct {
	svc     JWKSService
	limiter *rate.Limiter
}

// NewJWKSHandler constructs a JWKSHandler. No rate limit is applied unless
// WithRateLimit is also called.
func NewJWKSHandler(svc JWKSService) *JWKSHandler {
	return &JWKSHandler{svc: svc}
}

// WithRateLimit installs a process-wide (not per-tenant/per-caller) token
// bucket on the JWKS route and returns h for chaining — matches the
// eventbusadapter.New(...).WithLogger(...) / CredentialService.WithCadenceDays
// convention already used at this service's composition roots. This route
// is deliberately unauthenticated by header (its whole reason for
// existing, §2.5) and reads a private key out of OpenBao per live
// credential on every hit (JWKSService.PublicKeys), so — unlike every
// other route on this service, which is gated by caller identity — it has
// no other defense against being hit at an arbitrary rate by anything
// that can reach the pod. A single shared limiter (not one per tenant) is
// intentional: the concern is OpenBao read-amplification against this
// service as a whole, not any one tenant's fair share.
func (h *JWKSHandler) WithRateLimit(requestsPerSecond float64, burst int) *JWKSHandler {
	h.limiter = rate.NewLimiter(rate.Limit(requestsPerSecond), burst)
	return h
}

type jwksResponseBody struct {
	Keys []map[string]any `json:"keys"`
}

// JWKS implements EXT-6's JWKS-serving route: GET
// /api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json
// — deliberately registered outside the protected middleware group
// (router.go): Keycloak's own outbound client-jwt/jwks.url fetch (§2.5,
// RP-17) carries none of the x-user-id/x-tenant-id headers every other
// route requires, and gincommon.ProtectedMiddlewares fails closed with no
// leniency. RLS is still enforced — bound here from the trusted path
// parameter instead of a header, tenant-only (no UserID set), the same
// pattern GUCBridgeMiddleware uses for every other route, just sourced
// from the URL for this one necessarily-unauthenticated-by-header route.
//
// @Summary      EXT-6 — Platform-automation JWKS
// @Description  Public JWK Set for the tenant's platform-automation principal — the keys Keycloak's client-jwt authenticator fetches to verify that principal's client_assertion (§2.5). Always 200 with a (possibly empty) keys array; an unknown tenant or absent principal is never distinguished from a principal with zero live keys, since this route has no caller identity to authorize a 404 against.
// @Tags         ServiceAccounts
// @Produce      json
// @Param        id  path      string  true  "Tenant UUID"  format(uuid)
// @Success      200 {object}  jwksResponseBody
// @Failure      400 {object}  ErrorResponse  "invalid_request"
// @Failure      429 {object}  ErrorResponse  "rate_limited"
// @Failure      500 {object}  ErrorResponse
// @Router       /tenants/{id}/service-accounts/platform-automation/jwks.json [get]
func (h *JWKSHandler) JWKS(c *gin.Context) {
	if h.limiter != nil && !h.limiter.Allow() {
		writeError(c, "rate_limited", http.StatusTooManyRequests, nil)
		return
	}

	tenantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		writeInvalidRequest(c, "id", "path tenant id must be a UUID")
		return
	}

	ctx := pgcommon.WithGUCSet(c.Request.Context(), pgdomain.GUCSet{TenantID: tenantID.String()})

	keys, skipped, err := h.svc.PublicKeys(ctx, tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	// A skipped credential is a real per-credential auth outage (Keycloak
	// cannot validate that key) masquerading as a 200 — see PublicKeys' doc
	// comment. Recorded here, not inside the service layer, because
	// core/service may not depend on observability (.go-arch-lint.yml);
	// adapters_inbound may.
	if skipped > 0 {
		metrics.JWKSKeyErrorsTotal.Add(float64(skipped))
	}

	// A short cache is safe (content is public and only changes right when
	// RP calls ClearServiceAccountKeysCache, §2.5) and cheap insurance
	// against a client — including a misbehaving/looping Keycloak — hammering
	// this route: every hit reads OpenBao once per live credential
	// (JWKSService.PublicKeys), so serving a repeat fetch from cache instead
	// of re-reading OpenBao reduces that amplification without depending on
	// the rate limiter alone. Keycloak's own fetch is lazy/uncached at its
	// end regardless (it holds whatever it last fetched until explicitly
	// told to clear), so this header does not delay Keycloak noticing a
	// change — RP's cache-clear always triggers a fresh outbound fetch.
	c.Header("Cache-Control", "public, max-age=60")
	// This is a fully public, unauthenticated JSON endpoint (production-readiness
	// review, TS-D15) — nosniff costs nothing and closes off a MIME-sniffing
	// avenue that would otherwise be open on every other route (which are
	// all header-authenticated, so a browser is never a realistic caller).
	c.Header("X-Content-Type-Options", "nosniff")
	c.JSON(http.StatusOK, jwksResponseBody{Keys: keys})
}
