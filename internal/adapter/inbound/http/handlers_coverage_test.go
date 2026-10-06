package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ReadPlatformAutomation's own identity guard: called without the
// RequireIdentityHeaders/ProtectedMiddlewares chain (so no requestctx in the
// request context), it answers 401 missing_identity_headers itself and never
// reaches the service — a nil service would panic if it did.
func TestPrincipalHandler_ReadPlatformAutomation_NoRequestContextIs401(t *testing.T) {
	h := NewPrincipalHandler(nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/tenants/"+uuid.NewString()+"/service-accounts/platform-automation", nil)

	h.ReadPlatformAutomation(c)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, "missing_identity_headers", decodeErrorBody(t, rec).Error)
}

// WithLogger returns the same handler for chaining, and a failed refresh is
// reported to that logger (with the error) while the previous known set is
// kept.
func TestJWKSHandler_WithLogger_FailedRefreshIsLoggedAndKeepsSet(t *testing.T) {
	known := uuid.New()
	log := &fakeLogger{}
	h := NewJWKSHandler(&fakeJWKSService{})
	require.Same(t, h, h.WithLogger(log))

	src := &fakeKnownTenantSource{sets: [][]uuid.UUID{{known}}}
	h.refreshKnownTenants(context.Background(), src) // succeeds
	h.refreshKnownTenants(context.Background(), src) // script exhausted: "db down"

	require.Len(t, log.warnCalls, 1)
	assert.Equal(t, "db down", log.warnCalls[0]["error"])
	assert.True(t, h.known.has(known), "a failed refresh keeps the previous set")
}

// A failed refresh on an already-cancelled context is shutdown noise, not a
// database problem: nothing is logged.
func TestJWKSHandler_FailedRefreshAfterCancelIsNotLogged(t *testing.T) {
	log := &fakeLogger{}
	h := NewJWKSHandler(&fakeJWKSService{}).WithLogger(log)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.refreshKnownTenants(ctx, &fakeKnownTenantSource{})

	assert.Empty(t, log.warnCalls)
}

// A non-positive interval falls back to DefaultKnownTenantsRefresh instead of
// panicking in time.NewTicker; the initial refresh still runs immediately.
func TestJWKSHandler_RunKnownTenantRefresher_NonPositiveIntervalUsesDefault(t *testing.T) {
	tenant := uuid.New()
	h := NewJWKSHandler(&fakeJWKSService{})
	src := &fakeKnownTenantSource{sets: [][]uuid.UUID{{tenant}}}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { h.RunKnownTenantRefresher(ctx, src, 0); close(stopped) }()

	require.Eventually(t, func() bool { return h.known.has(tenant) }, 5*time.Second, time.Millisecond)
	cancel()
	<-stopped

	src.mu.Lock()
	defer src.mu.Unlock()
	assert.Equal(t, 1, src.calls, "only the immediate refresh ran: the default interval is not a zero-length ticker")
}

// A description longer than 200 characters is truncated with an ellipsis.
func TestRenderPropsTable_LongDescriptionIsTruncated(t *testing.T) {
	long := strings.Repeat("d", 250)
	sc := &asyncSchema{
		Properties:    map[string]asyncProp{"field": {Type: "string", Desc: long}},
		PropertyOrder: []string{"field"},
	}
	var buf bytes.Buffer
	renderPropsTable(&buf, sc, "X")
	out := buf.String()
	assert.Contains(t, out, strings.Repeat("d", 200)+"…")
	assert.NotContains(t, out, strings.Repeat("d", 201))
}
