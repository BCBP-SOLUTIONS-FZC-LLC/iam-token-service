package http

import (
	"container/list"
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
	pgdomain "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// JWKSService is the subset of *service.JWKSService this handler calls —
// mirrors PrincipalHandler/CredentialHandler's interface-not-concrete-type
// pattern for unit-testability against a hand-written fake. The result's
// Skipped/ActiveSkipped report live credentials whose key could not be
// read/parsed (TS-D15, TS-D23) — see service.JWKSService.PublicKeys.
type JWKSService interface {
	PublicKeys(ctx context.Context, tenantID uuid.UUID) (*service.JWKSResult, error)
}

// KnownTenantSource lists the tenants that currently have a live (active or
// rotating) credential — the authoritative "known tenant" set
// (postgres.JWKSTenantsRepository, read over the BYPASSRLS reconciler pool).
type KnownTenantSource interface {
	ListTenantsWithLiveCredentials(ctx context.Context) ([]uuid.UUID, error)
}

// JWKSHandler implements EXT-6's JWKS-serving route (§2.5).
//
// Rate limiting (TS-D22) — three token buckets, all optional and only
// active once WithRateLimit is called:
//   - per tenant (WithPerTenantRateLimit): one caller looping on one URL
//     cannot starve other tenants;
//   - global (WithRateLimit): spent ONLY by known tenants — tenants with a
//     live credential in the database (RunKnownTenantRefresher), plus any
//     tenant this replica served a live key to since the last refresh;
//   - unknown-tenant (WithUnknownTenantRateLimit, default 2 rps / burst 5):
//     one small shared bucket for every other tenant id.
//
// The split exists because the tenant id in the path is caller-chosen on an
// unauthenticated route: a flood of random tenant UUIDs used to get a fresh
// per-tenant bucket each and drain the global bucket, so Keycloak's real
// fetches for real tenants were the ones answered 429. Random ids can now
// only ever exhaust the unknown-tenant bucket.
//
// "Known" comes from the database, not from this replica's history
// (TS-D23): when it meant "served at least once in the last 10 minutes",
// a fresh replica (or a tenant Keycloak had not fetched for a while)
// started every real tenant in the unknown bucket, where a random-UUID
// flood could keep it from ever being served — and so from ever becoming
// known. Only a tenant with no live credential at all (brand new, or
// made up) now spends from the unknown bucket.
type JWKSHandler struct {
	svc JWKSService

	limiter        *rate.Limiter // global, known tenants only
	unknownLimiter *rate.Limiter // shared by all not-known tenants
	perTenant      *tenantLimiters
	known          *knownTenants

	log port.Logger // optional; RunKnownTenantRefresher's failure log
}

// Unknown-tenant bucket defaults (JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS /
// _BURST), applied when WithRateLimit is called without
// WithUnknownTenantRateLimit.
const (
	defaultUnknownTenantRPS   = 2
	defaultUnknownTenantBurst = 5
)

// DefaultKnownTenantsRefresh is RunKnownTenantRefresher's interval when it
// is given a non-positive one (JWKS_KNOWN_TENANTS_REFRESH's default).
const DefaultKnownTenantsRefresh = 15 * time.Second

// NewJWKSHandler constructs a JWKSHandler. No rate limit is applied unless
// WithRateLimit is also called.
func NewJWKSHandler(svc JWKSService) *JWKSHandler {
	return &JWKSHandler{svc: svc, known: newKnownTenants(maxTrackedTenants)}
}

// RunKnownTenantRefresher keeps the known-tenant set in step with the
// database: it refreshes immediately, then every interval
// (JWKS_KNOWN_TENANTS_REFRESH, default 15s), until ctx is done. Blocking —
// cmd/server runs it in its own goroutine. Each successful refresh
// REPLACES the set; tenants this replica served a live key to after the
// refresh's query started stay known until the next one, so a credential
// issued between two refreshes is not demoted to the unknown bucket. A
// failed refresh keeps the previous set (a database blip must not demote
// every real tenant at once) and is logged.
func (h *JWKSHandler) RunKnownTenantRefresher(ctx context.Context, src KnownTenantSource, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultKnownTenantsRefresh
	}
	h.refreshKnownTenants(ctx, src)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.refreshKnownTenants(ctx, src)
		}
	}
}

func (h *JWKSHandler) refreshKnownTenants(ctx context.Context, src KnownTenantSource) {
	started := h.known.now()
	ids, err := src.ListTenantsWithLiveCredentials(ctx)
	if err != nil {
		if ctx.Err() == nil && h.log != nil {
			h.log.Warn("jwks: known-tenant refresh failed; keeping the previous set", map[string]interface{}{
				"error": err.Error(),
			})
		}
		return
	}
	h.known.replace(ids, started)
}

// WithRateLimit installs the global token bucket on the JWKS route and
// returns h for chaining — matches the
// eventbusadapter.New(...).WithLogger(...) / CredentialService.WithCadenceDays
// convention already used at this service's composition roots. This route
// is deliberately unauthenticated by header (its whole reason for
// existing, §2.5) and reads a private key out of OpenBao per live
// credential on every hit (JWKSService.PublicKeys), so — unlike every
// other route on this service, which is gated by caller identity — it has
// no other defense against being hit at an arbitrary rate by anything
// that can reach the pod. Only known tenants spend from it; it also turns
// on the unknown-tenant bucket at its defaults unless
// WithUnknownTenantRateLimit set it.
func (h *JWKSHandler) WithRateLimit(requestsPerSecond float64, burst int) *JWKSHandler {
	h.limiter = rate.NewLimiter(rate.Limit(requestsPerSecond), burst)
	if h.unknownLimiter == nil {
		h.unknownLimiter = rate.NewLimiter(defaultUnknownTenantRPS, defaultUnknownTenantBurst)
	}
	return h
}

// WithLogger sets the logger RunKnownTenantRefresher reports a failed
// refresh to. Call it before starting the refresher.
func (h *JWKSHandler) WithLogger(log port.Logger) *JWKSHandler {
	h.log = log
	return h
}

// WithUnknownTenantRateLimit sets the small shared bucket every not-known
// tenant id spends from instead of the global one
// (JWKS_RATE_LIMIT_UNKNOWN_TENANT_RPS / _BURST, defaults 2 / 5).
func (h *JWKSHandler) WithUnknownTenantRateLimit(requestsPerSecond float64, burst int) *JWKSHandler {
	h.unknownLimiter = rate.NewLimiter(rate.Limit(requestsPerSecond), burst)
	return h
}

// WithPerTenantRateLimit adds a token bucket per tenant (keyed by the path
// tenant id) on top of the shared ones, so one caller looping on one
// tenant's URL cannot starve every other tenant's — above all Keycloak's
// post-RP-17 — fetches (JWKS_RATE_LIMIT_PER_TENANT_RPS / _BURST).
func (h *JWKSHandler) WithPerTenantRateLimit(requestsPerSecond float64, burst int) *JWKSHandler {
	h.perTenant = newTenantLimiters(rate.Limit(requestsPerSecond), burst, maxTrackedTenants)
	return h
}

// maxTrackedTenants bounds the per-tenant limiter and known-tenant LRUs: the
// route is unauthenticated, so the tenant id in the path is caller-chosen.
const maxTrackedTenants = 10000

// allow spends one token from the tenant's own bucket and one from the
// shared bucket its known/unknown status selects. Reservations, not Allow,
// so a request the second check denies hands its first token back — a
// denied request never costs a tenant (or the shared pool) anything. On a
// denial, bucket names the limiter that denied it (metrics.JWKSBucket*).
func (h *JWKSHandler) allow(tenantID uuid.UUID) (ok bool, bucket string) {
	shared, sharedName := h.unknownLimiter, metrics.JWKSBucketUnknown
	if h.known.has(tenantID) {
		shared, sharedName = h.limiter, metrics.JWKSBucketGlobal
	}
	// One timestamp for reserve and cancel: a reservation cancelled at a
	// later instant than it was made is (by x/time/rate's rules) already
	// "acted on" and restores nothing.
	now := time.Now()
	var reserved []*rate.Reservation
	release := func() {
		for _, r := range reserved {
			r.CancelAt(now)
		}
	}
	for _, c := range []struct {
		l    *rate.Limiter
		name string
	}{{h.perTenantLimiter(tenantID), metrics.JWKSBucketTenant}, {shared, sharedName}} {
		if c.l == nil {
			continue
		}
		r := c.l.ReserveN(now, 1)
		if !r.OK() || r.DelayFrom(now) > 0 {
			r.CancelAt(now)
			release()
			return false, c.name
		}
		reserved = append(reserved, r)
	}
	return true, ""
}

func (h *JWKSHandler) perTenantLimiter(tenantID uuid.UUID) *rate.Limiter {
	if h.perTenant == nil {
		return nil
	}
	return h.perTenant.get(tenantID)
}

// lru is a minimal bounded least-recently-used map keyed by tenant id. Not
// safe for concurrent use; callers hold their own mutex.
type lru[V any] struct {
	max   int
	order *list.List // front = most recently used; elements hold *lruEntry[V]
	items map[uuid.UUID]*list.Element
}

type lruEntry[V any] struct {
	key uuid.UUID
	val V
}

func newLRU[V any](maxEntries int) *lru[V] {
	return &lru[V]{max: maxEntries, order: list.New(), items: map[uuid.UUID]*list.Element{}}
}

func (l *lru[V]) get(key uuid.UUID) (V, bool) {
	if el, ok := l.items[key]; ok {
		l.order.MoveToFront(el)
		return el.Value.(*lruEntry[V]).val, true
	}
	var zero V
	return zero, false
}

func (l *lru[V]) put(key uuid.UUID, val V) {
	if el, ok := l.items[key]; ok {
		el.Value.(*lruEntry[V]).val = val
		l.order.MoveToFront(el)
		return
	}
	l.items[key] = l.order.PushFront(&lruEntry[V]{key: key, val: val})
	for l.order.Len() > l.max {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.items, oldest.Value.(*lruEntry[V]).key)
	}
}

func (l *lru[V]) len() int { return l.order.Len() }

// tenantLimiters is a bounded LRU of per-tenant token buckets. Eviction is
// least-recently-used — a tenant that is actively being hit (a flood, or
// Keycloak) keeps its bucket — rather than the old reset-on-full, which
// handed every tenant, the abusive one included, a fresh full burst each
// time the map filled.
type tenantLimiters struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	buckets *lru[*rate.Limiter]
}

func newTenantLimiters(limit rate.Limit, burst, maxTenants int) *tenantLimiters {
	return &tenantLimiters{limit: limit, burst: burst, buckets: newLRU[*rate.Limiter](maxTenants)}
}

func (t *tenantLimiters) get(tenantID uuid.UUID) *rate.Limiter {
	t.mu.Lock()
	defer t.mu.Unlock()
	l, ok := t.buckets.get(tenantID)
	if !ok {
		l = rate.NewLimiter(t.limit, t.burst)
		t.buckets.put(tenantID, l)
	}
	return l
}

// knownTenants is the set of tenants allowed to spend from the global
// bucket: the database's tenants-with-a-live-credential set (replaced
// wholesale by each refresh), plus a bounded LRU of tenants this replica
// served a live key to since the last refresh. Only a real tenant with a
// real credential can enter either part, so made-up ids can neither fill
// it nor evict a real tenant from it. No TTL: the refresh is what ages a
// tenant out (its last credential revoked, or the tenant offboarded).
type knownTenants struct {
	mu     sync.Mutex
	now    func() time.Time
	db     map[uuid.UUID]struct{}
	served *lru[time.Time] // tenant -> when it was last served
	max    int
}

func newKnownTenants(maxTenants int) *knownTenants {
	return &knownTenants{now: time.Now, db: map[uuid.UUID]struct{}{}, served: newLRU[time.Time](maxTenants), max: maxTenants}
}

func (k *knownTenants) has(tenantID uuid.UUID) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.db[tenantID]; ok {
		return true
	}
	_, ok := k.served.get(tenantID)
	return ok
}

func (k *knownTenants) mark(tenantID uuid.UUID) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.served.put(tenantID, k.now())
}

// replace installs ids as the database set and forgets every served-mark
// older than since (the refresh's query start): those tenants are in ids
// if they still have a live credential. A mark made while the query ran
// is kept, so it cannot be lost to a read that predates it.
func (k *knownTenants) replace(ids []uuid.UUID, since time.Time) {
	db := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		db[id] = struct{}{}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.db = db
	kept := newLRU[time.Time](k.max)
	// Back to front, so the rebuilt LRU keeps the same recency order.
	for el := k.served.order.Back(); el != nil; el = el.Prev() {
		e := el.Value.(*lruEntry[time.Time])
		if !e.val.Before(since) {
			kept.put(e.key, e.val)
		}
	}
	k.served = kept
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
// @Description  Public JWK Set for the tenant's platform-automation principal — the keys Keycloak's client-jwt authenticator fetches to verify that principal's client_assertion (§2.5). 200 with a (possibly empty) keys array; an unknown tenant or absent principal is never distinguished from a principal with zero live keys, since this route has no caller identity to authorize a 404 against. 503 jwks_keys_unavailable when the ACTIVE key, or every live key, failed to read from OpenBao (so Keycloak keeps its cached keys rather than caching a set without the current key); a set missing only overlap (rotating) keys is served as 200. No identity headers; rate-limited per tenant, and tenant ids with no live credential share a small separate bucket.
// @Tags         ServiceAccounts
// @Produce      json
// @Param        id  path      string  true  "Tenant UUID"  format(uuid)
// @Success      200 {object}  jwksResponseBody
// @Failure      400 {object}  ErrorResponse  "invalid_request"
// @Failure      429 {object}  ErrorResponse  "rate_limited"
// @Failure      500 {object}  ErrorResponse
// @Failure      503 {object}  ErrorResponse  "jwks_keys_unavailable (active key, or every live key, unreadable) | db_unavailable"
// @Router       /tenants/{id}/service-accounts/platform-automation/jwks.json [get]
func (h *JWKSHandler) JWKS(c *gin.Context) {
	tenantID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		writeInvalidRequest(c, "id", "path tenant id must be a UUID")
		return
	}
	if ok, bucket := h.allow(tenantID); !ok {
		// Every 429 is counted: a rate-limited Keycloak fetch right after
		// RP-17 is an auth outage for that tenant (iam_token_service_jwks_rate_limited_total).
		// The by-bucket split tells a random-id flood (unknown) from a
		// starved real tenant (tenant/global).
		if metrics.JWKSRateLimitedTotal != nil {
			metrics.JWKSRateLimitedTotal.Inc()
		}
		metrics.IncJWKSRateLimited(bucket)
		writeError(c, "rate_limited", http.StatusTooManyRequests, nil)
		return
	}

	ctx := pgcommon.WithGUCSet(c.Request.Context(), pgdomain.GUCSet{TenantID: tenantID.String()})

	res, err := h.svc.PublicKeys(ctx, tenantID)
	if err != nil {
		HandleError(c, err)
		return
	}
	keys, skipped := res.Keys, res.Skipped
	// A skipped credential is a real per-credential auth outage (Keycloak
	// cannot validate that key) masquerading as a 200 — see PublicKeys' doc
	// comment. Recorded here, not inside the service layer, because
	// core/service may not depend on observability (.go-arch-lint.yml);
	// adapters_inbound may.
	if skipped > 0 {
		metrics.JWKSKeyErrorsTotal.Add(float64(skipped))
	}
	if len(keys) > 0 || skipped > 0 {
		h.known.mark(tenantID)
	}
	// The active key, or every live key, failed to read (an OpenBao
	// outage, or lost material): a 200 would make Keycloak cache a set
	// without the key the automation principal signs with today and reject
	// every client_assertion for this tenant. A 5xx makes it keep the keys
	// it already has (TS-D23: jwks_keys_unavailable, 503 — not
	// secret_store_unavailable, whose frozen status is 502). A set missing
	// only overlap (rotating) keys is still served.
	if res.Unservable() {
		de := domain.NewError(domain.ErrJWKSKeysUnavailable, "the tenant's active JWKS key could not be read")
		writeError(c, string(de.Code), de.Status(), nil)
		return
	}

	// no-cache: an intermediary cache between Keycloak and this route could
	// otherwise hand Keycloak a key set that is up to max-age stale right
	// after RP-17 cleared its cache — still serving a revoked key, or missing
	// the new one — and Keycloak then keeps that stale set indefinitely.
	// OpenBao read amplification is bounded by the rate limiters instead.
	c.Header("Cache-Control", "no-cache")
	// This is a fully public, unauthenticated JSON endpoint (production-readiness
	// review, TS-D15) — nosniff costs nothing and closes off a MIME-sniffing
	// avenue that would otherwise be open on every other route (which are
	// all header-authenticated, so a browser is never a realistic caller).
	c.Header("X-Content-Type-Options", "nosniff")
	c.JSON(http.StatusOK, jwksResponseBody{Keys: keys})
}
