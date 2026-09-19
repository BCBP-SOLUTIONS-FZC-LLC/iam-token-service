package realmprovisioner_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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
// always sleeping out the full retryBackoff duration.
func TestClient_RefreshKeys_ContextCancelledDuringBackoffReturnsPromptly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	c := realmprovisioner.New(srv.URL, nil)
	start := time.Now()
	err := c.RefreshKeys(ctx, uuid.New())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 150*time.Millisecond, "must return once ctx is cancelled, not wait out the full backoff")
}
