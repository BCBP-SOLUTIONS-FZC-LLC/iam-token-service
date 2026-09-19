package http

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
)

// fakeJWKSService lets each test script a canned result/error — this
// package only verifies HTTP binding/status, not JWK derivation (that's
// test/unit/service's job).
type fakeJWKSService struct {
	keys    []map[string]any
	skipped int
	err     error

	gotTenantID uuid.UUID
}

func (f *fakeJWKSService) PublicKeys(_ context.Context, tenantID uuid.UUID) ([]map[string]any, int, error) {
	f.gotTenantID = tenantID
	return f.keys, f.skipped, f.err
}

var _ JWKSService = (*fakeJWKSService)(nil)

// newJWKSTestRouter deliberately mirrors registerJWKSRoutes, not
// newTenantScopedTestRouter — this route is registered outside the
// protected middleware group (router.go), so no identity headers are
// required or checked.
func newJWKSTestRouter(svc JWKSService) *gin.Engine {
	h := NewJWKSHandler(svc)
	r := gin.New()
	r.GET("/api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json", h.JWKS)
	return r
}

func TestJWKSHandler_JWKS(t *testing.T) {
	tenantID := uuid.New()

	t.Run("200 with keys, no identity headers required", func(t *testing.T) {
		svc := &fakeJWKSService{keys: []map[string]any{{"kty": "RSA", "kid": "k1"}}}
		r := newJWKSTestRouter(svc)
		var body jwksResponseBody
		rec := doJSON(t, r, http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation/jwks.json", nil, nil, &body)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Len(t, body.Keys, 1)
		assert.Equal(t, "k1", body.Keys[0]["kid"])
		assert.Equal(t, tenantID, svc.gotTenantID)
	})

	t.Run("200 with empty keys array for an unknown tenant", func(t *testing.T) {
		svc := &fakeJWKSService{keys: []map[string]any{}}
		r := newJWKSTestRouter(svc)
		var body jwksResponseBody
		rec := doJSON(t, r, http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation/jwks.json", nil, nil, &body)
		require.Equal(t, http.StatusOK, rec.Code, "an unknown tenant must never be distinguished from zero live keys")
		assert.Empty(t, body.Keys)
	})

	t.Run("400 non-UUID tenant id", func(t *testing.T) {
		svc := &fakeJWKSService{}
		r := newJWKSTestRouter(svc)
		rec := doJSON(t, r, http.MethodGet, "/api/v1/internal/tenants/not-a-uuid/service-accounts/platform-automation/jwks.json", nil, nil, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "id", er.Details["field"])
	})

	t.Run("500 on service error", func(t *testing.T) {
		svc := &fakeJWKSService{err: errors.New("db unavailable")}
		r := newJWKSTestRouter(svc)
		rec := doJSON(t, r, http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation/jwks.json", nil, nil, nil)
		require.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("200 sets a short Cache-Control header", func(t *testing.T) {
		svc := &fakeJWKSService{keys: []map[string]any{{"kty": "RSA", "kid": "k1"}}}
		r := newJWKSTestRouter(svc)
		rec := doJSON(t, r, http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation/jwks.json", nil, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "public, max-age=60", rec.Header().Get("Cache-Control"))
	})

	t.Run("200 sets X-Content-Type-Options: nosniff", func(t *testing.T) {
		svc := &fakeJWKSService{keys: []map[string]any{{"kty": "RSA", "kid": "k1"}}}
		r := newJWKSTestRouter(svc)
		rec := doJSON(t, r, http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation/jwks.json", nil, nil, nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	})
}

// TestJWKSHandler_RateLimit covers WithRateLimit — the production-readiness
// audit's finding that this unauthenticated, OpenBao-reading route had no
// defense against being hit at an arbitrary rate.
func TestJWKSHandler_RateLimit(t *testing.T) {
	tenantID := uuid.New()

	t.Run("no limiter installed never rate-limits", func(t *testing.T) {
		svc := &fakeJWKSService{keys: []map[string]any{}}
		r := newJWKSTestRouter(svc) // NewJWKSHandler without WithRateLimit
		for range 5 {
			rec := doJSON(t, r, http.MethodGet, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation/jwks.json", nil, nil, nil)
			require.Equal(t, http.StatusOK, rec.Code)
		}
	})

	t.Run("exhausting the burst returns 429, not a service call", func(t *testing.T) {
		svc := &fakeJWKSService{keys: []map[string]any{}}
		h := NewJWKSHandler(svc).WithRateLimit(0, 1) // 0 refill rate, burst of exactly 1
		r := gin.New()
		r.GET("/api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json", h.JWKS)

		path := "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/platform-automation/jwks.json"
		first := doJSON(t, r, http.MethodGet, path, nil, nil, nil)
		require.Equal(t, http.StatusOK, first.Code, "the burst's first token must still succeed")

		second := doJSON(t, r, http.MethodGet, path, nil, nil, nil)
		require.Equal(t, http.StatusTooManyRequests, second.Code)
		er := decodeErrorBody(t, second)
		assert.Equal(t, "rate_limited", er.Error)
	})
}

// TestJWKSHandler_RecordsSkippedKeysMetric covers the production-readiness
// audit's finding that a silently-partial JWKS response (a live credential
// whose key couldn't be read) reached only a log line, never anything
// page-worthy. The handler — not the service, which may not depend on
// observability (.go-arch-lint.yml) — is responsible for surfacing it.
func TestJWKSHandler_RecordsSkippedKeysMetric(t *testing.T) {
	metrics.Register("test")
	before := testutil.ToFloat64(metrics.JWKSKeyErrorsTotal)

	svc := &fakeJWKSService{keys: []map[string]any{}, skipped: 2}
	r := newJWKSTestRouter(svc)
	rec := doJSON(t, r, http.MethodGet, "/api/v1/internal/tenants/"+uuid.New().String()+"/service-accounts/platform-automation/jwks.json", nil, nil, nil)
	require.Equal(t, http.StatusOK, rec.Code, "a skip degrades gracefully — still 200 with whatever keys succeeded")

	after := testutil.ToFloat64(metrics.JWKSKeyErrorsTotal)
	assert.Equal(t, before+2, after)
}
