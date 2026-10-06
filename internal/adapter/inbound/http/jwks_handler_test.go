package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// fakeJWKSService lets each test script a canned result/error — this
// package only verifies HTTP binding/status, not JWK derivation (that's
// test/unit/service's job).
type fakeJWKSService struct {
	keys          []map[string]any
	skipped       int
	activeSkipped bool
	err           error

	gotTenantID uuid.UUID
}

func (f *fakeJWKSService) PublicKeys(_ context.Context, tenantID uuid.UUID) (*service.JWKSResult, error) {
	f.gotTenantID = tenantID
	if f.err != nil {
		return nil, f.err
	}
	return &service.JWKSResult{Keys: f.keys, Skipped: f.skipped, ActiveSkipped: f.activeSkipped}, nil
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
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), "no intermediary may serve a stale key set after RP-17")
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
		h.known.mark(tenantID)                       // the global bucket is spent by known tenants
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

	t.Run("per-tenant limit isolates tenants and every 429 is counted", func(t *testing.T) {
		metrics.Register("test")
		before := testutil.ToFloat64(metrics.JWKSRateLimitedTotal)
		svc := &fakeJWKSService{keys: []map[string]any{}}
		h := NewJWKSHandler(svc).WithRateLimit(1000, 1000).WithPerTenantRateLimit(0, 1)
		r := gin.New()
		r.GET("/api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json", h.JWKS)
		pathFor := func(id uuid.UUID) string {
			return "/api/v1/internal/tenants/" + id.String() + "/service-accounts/platform-automation/jwks.json"
		}
		flooded, other := uuid.New(), uuid.New()

		require.Equal(t, http.StatusOK, doJSON(t, r, http.MethodGet, pathFor(flooded), nil, nil, nil).Code)
		require.Equal(t, http.StatusTooManyRequests, doJSON(t, r, http.MethodGet, pathFor(flooded), nil, nil, nil).Code)
		assert.Equal(t, http.StatusOK, doJSON(t, r, http.MethodGet, pathFor(other), nil, nil, nil).Code,
			"a flood on one tenant's URL must not starve another tenant")
		assert.InDelta(t, before+1, testutil.ToFloat64(metrics.JWKSRateLimitedTotal), 0)
	})
}

func TestTenantLimiters_BoundedMemory(t *testing.T) {
	l := newTenantLimiters(1, 1, 3)
	for range 10 {
		l.get(uuid.New())
	}
	assert.Equal(t, 3, l.buckets.len())
}

// LRU, not reset-on-full: a hot tenant touched between floods of new ids
// keeps its (drained) bucket instead of being handed a fresh burst.
func TestTenantLimiters_LRUKeepsHotTenantBucket(t *testing.T) {
	l := newTenantLimiters(0, 1, 3)
	hot := uuid.New()
	require.True(t, l.get(hot).Allow())
	for range 10 {
		l.get(uuid.New())
		l.get(hot) // still in use
	}
	assert.False(t, l.get(hot).Allow(), "the hot tenant's drained bucket survived; no fresh burst")
	assert.Equal(t, 3, l.buckets.len())
}

func TestLRU_EvictsLeastRecentlyUsed(t *testing.T) {
	c := newLRU[int](2)
	a, b, d := uuid.New(), uuid.New(), uuid.New()
	c.put(a, 1)
	c.put(b, 2)
	_, _ = c.get(a) // a is now most recent
	c.put(d, 3)     // evicts b
	_, okB := c.get(b)
	va, okA := c.get(a)
	assert.False(t, okB)
	assert.True(t, okA)
	assert.Equal(t, 1, va)
	c.put(a, 9)
	va, _ = c.get(a)
	assert.Equal(t, 9, va)
}

// The database set is replaced wholesale; a served-mark survives only if it
// was made after the refresh's query started.
func TestKnownTenants_ReplaceSemantics(t *testing.T) {
	k := newKnownTenants(10)
	now := time.Now()
	k.now = func() time.Time { return now }
	dbA, dbB, servedOld, servedNew := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	k.mark(servedOld)
	k.replace([]uuid.UUID{dbA}, now)
	assert.True(t, k.has(dbA))
	assert.True(t, k.has(servedOld), "marked at the query start: kept")

	now = now.Add(time.Second)
	since := now
	now = now.Add(time.Second)
	k.mark(servedNew) // served while the next refresh's query ran
	k.replace([]uuid.UUID{dbB}, since)
	assert.False(t, k.has(dbA), "the refresh REPLACES the database set")
	assert.True(t, k.has(dbB))
	assert.False(t, k.has(servedOld), "a mark older than the refresh is dropped")
	assert.True(t, k.has(servedNew), "a mark made during the refresh is kept until the next one")
}

func jwksPath(id uuid.UUID) string {
	return "/api/v1/internal/tenants/" + id.String() + "/service-accounts/platform-automation/jwks.json"
}

// tenantKeysJWKSService serves one key for tenants in withKeys, none for
// any other id (an unknown/made-up tenant).
type tenantKeysJWKSService struct{ withKeys map[uuid.UUID]bool }

func (f *tenantKeysJWKSService) PublicKeys(_ context.Context, tenantID uuid.UUID) (*service.JWKSResult, error) {
	if f.withKeys[tenantID] {
		return &service.JWKSResult{Keys: []map[string]any{{"kty": "RSA", "kid": tenantID.String()}}}, nil
	}
	return &service.JWKSResult{Keys: []map[string]any{}}, nil
}

// A flood of random (unknown) tenant ids drains only the unknown-tenant
// bucket — a known tenant, i.e. Keycloak's real fetches, still gets 200.
func TestJWKSHandler_UnknownTenantFloodDoesNotStarveKnownTenant(t *testing.T) {
	real := uuid.New()
	svc := &tenantKeysJWKSService{withKeys: map[uuid.UUID]bool{real: true}}
	h := NewJWKSHandler(svc).WithRateLimit(0, 3).WithUnknownTenantRateLimit(0, 2).WithPerTenantRateLimit(1000, 1000)
	r := gin.New()
	r.GET("/api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json", h.JWKS)

	// First fetch for the real tenant is "unknown" (one unknown token), and
	// marks it known.
	require.Equal(t, http.StatusOK, doJSON(t, r, http.MethodGet, jwksPath(real), nil, nil, nil).Code)

	// Flood: the remaining unknown token, then 429s.
	limited := 0
	for range 50 {
		if doJSON(t, r, http.MethodGet, jwksPath(uuid.New()), nil, nil, nil).Code == http.StatusTooManyRequests {
			limited++
		}
	}
	assert.Equal(t, 49, limited, "random tenant ids share one small bucket")

	// The known tenant spends the untouched global bucket (burst 3).
	for range 3 {
		assert.Equal(t, http.StatusOK, doJSON(t, r, http.MethodGet, jwksPath(real), nil, nil, nil).Code)
	}
}

// A request the shared bucket denies hands its per-tenant token back.
func TestJWKSHandler_DeniedRequestDoesNotSpendPerTenantToken(t *testing.T) {
	svc := &fakeJWKSService{keys: []map[string]any{}}
	h := NewJWKSHandler(svc).WithRateLimit(1000, 1000).WithUnknownTenantRateLimit(0, 0).WithPerTenantRateLimit(0, 1)
	tenantID := uuid.New()

	ok, bucket := h.allow(tenantID)
	assert.False(t, ok, "unknown bucket of burst 0 denies")
	assert.Equal(t, metrics.JWKSBucketUnknown, bucket)
	assert.True(t, h.perTenant.get(tenantID).Allow(), "the per-tenant token was returned, not wasted")
}

func TestJWKSHandler_UnknownTenantBucketDefaults(t *testing.T) {
	h := NewJWKSHandler(&fakeJWKSService{}).WithRateLimit(10, 10)
	require.NotNil(t, h.unknownLimiter)
	assert.Equal(t, rate.Limit(2), h.unknownLimiter.Limit())
	assert.Equal(t, 5, h.unknownLimiter.Burst())

	h = NewJWKSHandler(&fakeJWKSService{}).WithUnknownTenantRateLimit(1, 3).WithRateLimit(10, 10)
	assert.Equal(t, rate.Limit(1), h.unknownLimiter.Limit(), "an explicit setting is not overwritten by the default")
	assert.Equal(t, 3, h.unknownLimiter.Burst())
}

// TestJWKSHandler_RecordsSkippedKeysMetric covers the production-readiness
// audit's finding that a silently-partial JWKS response (a live credential
// whose key couldn't be read) reached only a log line, never anything
// page-worthy. The handler — not the service, which may not depend on
// observability (.go-arch-lint.yml) — is responsible for surfacing it.
func TestJWKSHandler_RecordsSkippedKeysMetric(t *testing.T) {
	metrics.Register("test")
	before := testutil.ToFloat64(metrics.JWKSKeyErrorsTotal)

	svc := &fakeJWKSService{keys: []map[string]any{{"kty": "RSA", "kid": "k1"}}, skipped: 2}
	r := newJWKSTestRouter(svc)
	var body jwksResponseBody
	rec := doJSON(t, r, http.MethodGet, jwksPath(uuid.New()), nil, nil, &body)
	require.Equal(t, http.StatusOK, rec.Code, "a partial skip degrades gracefully — still 200 with whatever keys succeeded")
	assert.Len(t, body.Keys, 1)

	after := testutil.ToFloat64(metrics.JWKSKeyErrorsTotal)
	assert.Equal(t, before+2, after)
}

// Every live credential unreadable: 503, so Keycloak keeps its cached keys
// instead of caching an empty set — and the metric still counts them.
func TestJWKSHandler_AllKeysUnreadableIs503(t *testing.T) {
	metrics.Register("test")
	before := testutil.ToFloat64(metrics.JWKSKeyErrorsTotal)

	r := newJWKSTestRouter(&fakeJWKSService{keys: []map[string]any{}, skipped: 2})
	rec := doJSON(t, r, http.MethodGet, jwksPath(uuid.New()), nil, nil, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	er := decodeErrorBody(t, rec)
	assert.Equal(t, "jwks_keys_unavailable", er.Error, "not secret_store_unavailable, whose frozen status is 502")
	assert.Equal(t, http.StatusServiceUnavailable, er.Status)
	assert.Equal(t, before+2, testutil.ToFloat64(metrics.JWKSKeyErrorsTotal))
}

// TS-D23: an unreadable ACTIVE key is 503 even though an overlap key is
// readable — serving only the old key would make Keycloak cache a set that
// rejects today's client_assertion.
func TestJWKSHandler_ActiveKeyUnreadableIs503(t *testing.T) {
	metrics.Register("test")
	before := testutil.ToFloat64(metrics.JWKSKeyErrorsTotal)

	r := newJWKSTestRouter(&fakeJWKSService{keys: []map[string]any{{"kty": "RSA", "kid": "old"}}, skipped: 1, activeSkipped: true})
	rec := doJSON(t, r, http.MethodGet, jwksPath(uuid.New()), nil, nil, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "jwks_keys_unavailable", decodeErrorBody(t, rec).Error)
	assert.Equal(t, before+1, testutil.ToFloat64(metrics.JWKSKeyErrorsTotal))
}

// fakeKnownTenantSource returns a scripted set per call, and an error once
// the script runs out (so a failed refresh can be observed).
type fakeKnownTenantSource struct {
	mu    sync.Mutex
	sets  [][]uuid.UUID
	calls int
	done  chan struct{}
}

func (f *fakeKnownTenantSource) ListTenantsWithLiveCredentials(context.Context) ([]uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.done != nil && f.calls == len(f.sets)+1 {
		close(f.done)
	}
	if f.calls > len(f.sets) {
		return nil, errors.New("db down")
	}
	return f.sets[f.calls-1], nil
}

// A tenant in the database set is known on a fresh handler after the first
// refresh, though this replica never served it — and a random-UUID flood
// that has drained the unknown bucket cannot touch it.
func TestJWKSHandler_DBKnownTenantSurvivesUnknownFlood(t *testing.T) {
	metrics.Register("test")
	real := uuid.New()
	svc := &tenantKeysJWKSService{withKeys: map[uuid.UUID]bool{real: true}}
	h := NewJWKSHandler(svc).WithRateLimit(0, 3).WithUnknownTenantRateLimit(0, 1).WithPerTenantRateLimit(1000, 1000)
	src := &fakeKnownTenantSource{sets: [][]uuid.UUID{{real}}, done: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { h.RunKnownTenantRefresher(ctx, src, time.Millisecond); close(stopped) }()
	<-src.done // first refresh succeeded; the second failed and kept the set
	cancel()
	<-stopped
	assert.True(t, h.known.has(real), "known from the database, never served; a failed refresh keeps the set")

	r := gin.New()
	r.GET("/api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json", h.JWKS)
	beforeUnknown := testutil.ToFloat64(metrics.JWKSRateLimitedByBucketTotal.WithLabelValues(metrics.JWKSBucketUnknown))
	for range 50 {
		doJSON(t, r, http.MethodGet, jwksPath(uuid.New()), nil, nil, nil)
	}
	assert.InDelta(t, beforeUnknown+49, testutil.ToFloat64(metrics.JWKSRateLimitedByBucketTotal.WithLabelValues(metrics.JWKSBucketUnknown)), 0,
		"the flood is counted against the unknown bucket")
	for range 3 {
		assert.Equal(t, http.StatusOK, doJSON(t, r, http.MethodGet, jwksPath(real), nil, nil, nil).Code,
			"the DB-known tenant was never in the unknown bucket")
	}
	beforeGlobal := testutil.ToFloat64(metrics.JWKSRateLimitedByBucketTotal.WithLabelValues(metrics.JWKSBucketGlobal))
	require.Equal(t, http.StatusTooManyRequests, doJSON(t, r, http.MethodGet, jwksPath(real), nil, nil, nil).Code)
	assert.InDelta(t, beforeGlobal+1, testutil.ToFloat64(metrics.JWKSRateLimitedByBucketTotal.WithLabelValues(metrics.JWKSBucketGlobal)), 0)
}

// Each refresh replaces the set: a tenant dropped from the database (last
// credential revoked) goes back to the unknown bucket.
func TestJWKSHandler_RefreshReplacesKnownSet(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	h := NewJWKSHandler(&fakeJWKSService{})
	src := &fakeKnownTenantSource{sets: [][]uuid.UUID{{a}, {b}}}
	h.refreshKnownTenants(context.Background(), src)
	assert.True(t, h.known.has(a))
	h.refreshKnownTenants(context.Background(), src)
	assert.False(t, h.known.has(a))
	assert.True(t, h.known.has(b))
}

// A per-tenant denial is labelled bucket="tenant".
func TestJWKSHandler_AllowReportsDenyingBucket(t *testing.T) {
	id := uuid.New()
	h := NewJWKSHandler(&fakeJWKSService{}).WithRateLimit(1000, 1000).WithPerTenantRateLimit(0, 1)
	ok, _ := h.allow(id)
	require.True(t, ok)
	ok, bucket := h.allow(id)
	assert.False(t, ok)
	assert.Equal(t, metrics.JWKSBucketTenant, bucket)
}

// A cancelled request is not a key error: the service returns ctx.Err(), and
// the handler neither counts it nor writes a 5xx.
func TestJWKSHandler_CanceledRequestIsNotAKeyError(t *testing.T) {
	metrics.Register("test")
	before := testutil.ToFloat64(metrics.JWKSKeyErrorsTotal)

	h := NewJWKSHandler(&fakeJWKSService{err: context.Canceled})
	r := gin.New()
	r.GET("/api/v1/internal/tenants/:id/service-accounts/platform-automation/jwks.json", h.JWKS)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, jwksPath(uuid.New()), nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	assert.NotEqual(t, http.StatusInternalServerError, rec.Code)
	assert.NotEqual(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, before, testutil.ToFloat64(metrics.JWKSKeyErrorsTotal))
}
