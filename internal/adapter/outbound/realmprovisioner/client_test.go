package realmprovisioner_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/realmprovisioner"
)

func TestClient_RefreshKeys_Success(t *testing.T) {
	tenantID := uuid.New()
	var gotPath, gotMethod, gotUserID, gotTenantID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotUserID = r.Header.Get("x-user-id")
		gotTenantID = r.Header.Get("x-tenant-id")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := realmprovisioner.New(srv.URL, nil)
	err := c.RefreshKeys(context.Background(), tenantID)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/internal/tenants/"+tenantID.String()+"/service-account/keys/refresh", gotPath)
	assert.Equal(t, "00000000-0000-0000-0000-0000000000a1", gotUserID)
	assert.Equal(t, tenantID.String(), gotTenantID)
}

func TestClient_RefreshKeys_UnexpectedStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity) // service_account_not_provisioned
	}))
	defer srv.Close()

	c := realmprovisioner.New(srv.URL, nil)
	err := c.RefreshKeys(context.Background(), uuid.New())
	require.Error(t, err)
}

func TestClient_RefreshKeys_UnreachableReturnsError(t *testing.T) {
	c := realmprovisioner.New("http://127.0.0.1:0", nil)
	err := c.RefreshKeys(context.Background(), uuid.New())
	require.Error(t, err)
}

// TestClient_RefreshKeys_RetriesOn502ThenSucceeds covers the
// production-readiness audit's finding that RefreshKeys never retried —
// RP-17 is documented idempotent (clearing an already-fresh cache is a
// no-op), so a transient 502 (keycloak_unavailable) should not fail the
// whole cmd/scheduler/cmd/rotator run outright.
func TestClient_RefreshKeys_RetriesOn502ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 1 {
			w.WriteHeader(http.StatusBadGateway) // keycloak_unavailable
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := realmprovisioner.New(srv.URL, nil)
	err := c.RefreshKeys(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "must succeed on the 2nd (final) attempt")
}

// TestClient_RefreshKeys_ExhaustsRetriesOnPersistent502 covers the other
// half: retries are bounded (2 attempts total — production-readiness
// review, TS-D15: kept to exactly one retry so RefreshKeys' worst-case
// per-row cost stays small against the CronJob's activeDeadlineSeconds
// budget), not infinite.
func TestClient_RefreshKeys_ExhaustsRetriesOnPersistent502(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := realmprovisioner.New(srv.URL, nil)
	err := c.RefreshKeys(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "must stop after exactly 2 attempts")
}

// TestClient_RefreshKeys_422DoesNotRetry covers the other classification
// branch: a permanent business error (service_account_not_provisioned)
// fails on the first attempt — retrying it cannot help.
func TestClient_RefreshKeys_422DoesNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	c := realmprovisioner.New(srv.URL, nil)
	err := c.RefreshKeys(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "a permanent error must not be retried")
}

// TestClient_RefreshKeys_ContextCancelledDuringBackoffReturnsPromptly
// covers the backoff wait itself respecting ctx cancellation rather than
// always sleeping out the full retryBackoff duration. No wall-clock bound:
// the ctx is cancelled by the first (retryable) attempt, and the backoff
// branch is the only one returning the bare ctx.Err() — a second attempt
// would have reached the server (calls == 2) or returned a wrapped
// "unreachable" error.
func TestClient_RefreshKeys_ContextCancelledDuringBackoffReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
		cancel()
	}))
	defer srv.Close()

	c := realmprovisioner.New(srv.URL, nil)
	err := c.RefreshKeys(ctx, uuid.New())

	require.Error(t, err)
	assert.Equal(t, context.Canceled, err, "returned from the backoff wait, not from a second attempt")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "no second attempt after cancellation")
}

// Mesh/proxy-level transients are retried once, like RP-17's own 502: a
// post-commit RP-17 failure only self-heals on a later CronJob run, via
// keys_refresh_pending (TS-D22), and still pages.
func TestClient_RefreshKeys_RetriesTransientProxyStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&calls, 1) == 1 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			require.NoError(t, realmprovisioner.New(srv.URL, nil).RefreshKeys(context.Background(), uuid.New()))
			assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
		})
	}
}

func TestClient_RefreshKeys_Any2xxIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"refreshed"}`))
	}))
	defer srv.Close()

	assert.NoError(t, realmprovisioner.New(srv.URL, nil).RefreshKeys(context.Background(), uuid.New()))
}
