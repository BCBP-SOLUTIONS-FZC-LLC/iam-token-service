package openbao

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	openbaoapi "github.com/openbao/openbao/api/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
)

// TestMountRelativePath guards against the KVv2 SDK double-mount-prefix bug
// (see mountRelativePath's doc comment): every stored/returned openbao_path
// includes the mount segment ("iam/...", §25 frozen shape), but the SDK
// re-adds the mount itself, so this must be stripped before any KVv2 call.
// White-box (package openbao, not test/unit) because mountRelativePath is
// unexported — there is no way to exercise it from an external test
// package.
func TestMountRelativePath(t *testing.T) {
	c := &Client{cfg: Config{KVMount: "iam"}}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "strips the mount prefix",
			in:   "iam/serviceaccount/11111111-1111-1111-1111-111111111111/platform-automation/v1",
			want: "serviceaccount/11111111-1111-1111-1111-111111111111/platform-automation/v1",
		},
		{
			name: "strips the mount prefix from a list prefix (no trailing version segment)",
			in:   "iam/serviceaccount/11111111-1111-1111-1111-111111111111/platform-automation",
			want: "serviceaccount/11111111-1111-1111-1111-111111111111/platform-automation",
		},
		{
			name: "leaves an already-mount-relative path untouched",
			in:   "serviceaccount/11111111-1111-1111-1111-111111111111/platform-automation/v1",
			want: "serviceaccount/11111111-1111-1111-1111-111111111111/platform-automation/v1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.mountRelativePath(tc.in)
			if got != tc.want {
				t.Errorf("mountRelativePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// ── isNotFound / wrapErr — pure functions, tested directly ────────────────

func TestIsNotFound(t *testing.T) {
	t.Run("404 response error", func(t *testing.T) {
		err := &openbaoapi.ResponseError{StatusCode: 404, Errors: []string{"not found"}}
		assert.True(t, isNotFound(err))
	})
	t.Run("non-404 response error", func(t *testing.T) {
		err := &openbaoapi.ResponseError{StatusCode: 500, Errors: []string{"boom"}}
		assert.False(t, isNotFound(err))
	})
	t.Run("non-response error", func(t *testing.T) {
		assert.False(t, isNotFound(errors.New("plain error")))
	})
	t.Run("nil error", func(t *testing.T) {
		assert.False(t, isNotFound(nil))
	})
}

func TestWrapErr(t *testing.T) {
	err := wrapErr("write_secret", errors.New("boom"))
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
	assert.Contains(t, de.Message, "write_secret")
	assert.Contains(t, de.Message, "boom")
}

// ── fake OpenBao HTTP server ────────────────────────────────────────────
//
// Fakes just enough of OpenBao's HTTP API (Kubernetes-auth login + KV v2
// data/metadata endpoints) to drive every branch in client.go, without a
// real OpenBao binary or Docker — that's what test/integration/ is for
// (a real testcontainer, built separately).

type fakeBaoServer struct {
	srv *httptest.Server

	loginCalls  atomic.Int32
	loginStatus int
	loginBody   string // raw JSON body; if empty, a default success body is sent

	// per-route canned responses: status 0 means "use the default success
	// body for that route"; a non-zero status with a body overrides it.
	writeStatus int
	writeBody   string

	readStatus int
	readBody   string

	deleteStatus int
	deleteBody   string

	listStatus int
	listBody   string
}

func newFakeBaoServer(t *testing.T) *fakeBaoServer {
	t.Helper()
	f := &fakeBaoServer{loginStatus: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBaoServer) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v1/auth/kubernetes/login": // Logical().Write issues PUT, not POST
		f.loginCalls.Add(1)
		if f.loginBody != "" {
			writeJSON(w, f.loginStatus, f.loginBody)
			return
		}
		writeJSON(w, f.loginStatus, `{"auth":{"client_token":"fake-token","lease_duration":3600}}`)

	case r.URL.Path == "/v1/iam/data/serviceaccount/write": // Logical().Write issues PUT, not POST
		if f.writeBody != "" {
			writeJSON(w, statusOr(f.writeStatus, http.StatusOK), f.writeBody)
			return
		}
		writeJSON(w, statusOr(f.writeStatus, http.StatusOK),
			`{"data":{"version":1,"created_time":"2024-01-01T00:00:00Z","deletion_time":"","destroyed":false}}`)

	case r.Method == http.MethodGet && r.URL.Path == "/v1/iam/data/serviceaccount/read":
		if f.readBody != "" {
			writeJSON(w, statusOr(f.readStatus, http.StatusOK), f.readBody)
			return
		}
		writeJSON(w, statusOr(f.readStatus, http.StatusOK),
			`{"data":{"data":{"secret":"s3cr3t"},"metadata":{"version":1,"created_time":"2024-01-01T00:00:00Z","deletion_time":"","destroyed":false}}}`)

	case r.Method == http.MethodDelete && r.URL.Path == "/v1/iam/metadata/serviceaccount/delete":
		if f.deleteBody != "" {
			writeJSON(w, statusOr(f.deleteStatus, http.StatusNoContent), f.deleteBody)
			return
		}
		w.WriteHeader(statusOr(f.deleteStatus, http.StatusNoContent))

	case r.Method == http.MethodGet && r.URL.Path == "/v1/iam/metadata/serviceaccount/list":
		if f.listBody != "" {
			writeJSON(w, statusOr(f.listStatus, http.StatusOK), f.listBody)
			return
		}
		writeJSON(w, statusOr(f.listStatus, http.StatusOK), `{"data":{"keys":["v1","v2"]}}`)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func statusOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// writeTokenFile writes jwt to a temp file and returns its path, standing
// in for the pod's projected ServiceAccount token volume.
func writeTokenFile(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func newTestClient(t *testing.T, addr string) *Client {
	t.Helper()
	c, err := New(Config{
		Addr:                addr,
		AuthRole:            "iam-token-service",
		KVMount:             "iam",
		KubernetesTokenPath: writeTokenFile(t, "fake-jwt"),
	}, nil)
	require.NoError(t, err)
	// The SDK retries 5xx responses (default 2 retries with backoff) —
	// fine in production, but it turns every error-path test here into a
	// multi-second sleep for no additional coverage. Disable it for tests.
	c.baoClient.SetMaxRetries(0)
	return c
}

// ── New / Health ──────────────────────────────────────────────────────────

func TestNew_DefaultsTokenPathAndTimeout(t *testing.T) {
	c, err := New(Config{Addr: "http://127.0.0.1:1"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/var/run/secrets/kubernetes.io/serviceaccount/token", c.cfg.KubernetesTokenPath)
	assert.Equal(t, 8*time.Second, c.cfg.HTTPTimeout)
}

func TestNew_ClientConstructionFailure(t *testing.T) {
	// A control character in the address is invalid in an HTTP header
	// value and a malformed URL, which openbaoapi.NewClient rejects at
	// construction time — the one branch New() itself can fail on.
	_, err := New(Config{Addr: "http://[::1]:namedport"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "openbao: new client")
}

func TestNew_HonorsExplicitConfig(t *testing.T) {
	c, err := New(Config{
		Addr: "http://127.0.0.1:1", KubernetesTokenPath: "/custom/path", HTTPTimeout: 3 * time.Second,
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, "/custom/path", c.cfg.KubernetesTokenPath)
	assert.Equal(t, 3*time.Second, c.cfg.HTTPTimeout)
}

func TestHealth_Success(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)
	require.NoError(t, c.Health(t.Context()))
}

func TestHealth_LoginFailure(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.loginStatus = http.StatusForbidden
	srv.loginBody = `{"errors":["permission denied"]}`
	c := newTestClient(t, srv.srv.URL)
	err := c.Health(t.Context())
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
}

// ── token() ──────────────────────────────────────────────────────────────

func TestToken_MissingTokenFile(t *testing.T) {
	c, err := New(Config{Addr: "http://127.0.0.1:1", KubernetesTokenPath: filepath.Join(t.TempDir(), "missing")}, nil)
	require.NoError(t, err)
	_, terr := c.token(t.Context())
	require.Error(t, terr)
	var de *domain.Error
	require.ErrorAs(t, terr, &de)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
	assert.Contains(t, de.Message, "read_kubernetes_token")
}

func TestToken_EmptyAuthResponse(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.loginBody = `{}`
	c := newTestClient(t, srv.srv.URL)
	_, err := c.token(t.Context())
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Contains(t, de.Message, "kubernetes_login")
}

func TestToken_CachesUntilExpiry(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)

	_, err := c.token(t.Context())
	require.NoError(t, err)
	_, err = c.token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int32(1), srv.loginCalls.Load(), "second call should reuse the cached token")
}

func TestToken_ReLoginsWhenCacheExpired(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)
	// White-box: force an already-expired cache entry directly.
	c.cachedToken = "stale-token"
	c.expiresAt = time.Now().Add(-time.Minute)

	_, err := c.token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int32(1), srv.loginCalls.Load())
	assert.Equal(t, "fake-token", c.cachedToken)
}

// ── Write / Read / Delete / List ────────────────────────────────────────

func TestWrite_Success(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)
	require.NoError(t, c.Write(t.Context(), "iam/serviceaccount/write", "s3cr3t"))
}

func TestWrite_KVFailure(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.writeStatus = http.StatusInternalServerError
	srv.writeBody = `{"errors":["internal error"]}`
	c := newTestClient(t, srv.srv.URL)
	err := c.Write(t.Context(), "iam/serviceaccount/write", "s3cr3t")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
	assert.Contains(t, de.Message, "write_secret")
}

func TestWrite_LoginFailurePropagates(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.loginStatus = http.StatusInternalServerError
	srv.loginBody = `{"errors":["down"]}`
	c := newTestClient(t, srv.srv.URL)
	err := c.Write(t.Context(), "iam/serviceaccount/write", "s3cr3t")
	require.Error(t, err)
}

func TestRead_Success(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)
	got, err := c.Read(t.Context(), "iam/serviceaccount/read")
	require.NoError(t, err)
	assert.Equal(t, "s3cr3t", got)
}

func TestRead_NotFoundEmptyData(t *testing.T) {
	srv := newFakeBaoServer(t)
	// A 404 with an empty body maps to (nil secret, nil error) inside
	// ReadWithContext, but kv.Get itself then treats a nil secret as
	// ErrSecretNotFound — client.Read wraps whatever error kv.Get returns
	// under the "read_secret" op regardless of which of the two
	// not-found shapes produced it (see TestRead_SoftDeletedVersion for
	// the other one: secret non-nil but Data nil).
	srv.readStatus = http.StatusNotFound
	srv.readBody = "{}"
	c := newTestClient(t, srv.srv.URL)
	_, err := c.Read(t.Context(), "iam/serviceaccount/read")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
	assert.Contains(t, de.Message, "read_secret")
}

// TestRead_SoftDeletedVersion exercises client.Read's own
// "secret.Data == nil" branch (distinct from the SDK-level ErrSecretNotFound
// case above): a KV v2 version that was soft-deleted returns 200 with
// metadata but a null data block.
func TestRead_SoftDeletedVersion(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.readBody = `{"data":{"data":null,"metadata":{"version":1,"created_time":"2024-01-01T00:00:00Z","deletion_time":"2024-01-02T00:00:00Z","destroyed":false}}}`
	c := newTestClient(t, srv.srv.URL)
	_, err := c.Read(t.Context(), "iam/serviceaccount/read")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Contains(t, de.Message, "no secret at path")
}

func TestRead_MissingSecretField(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.readBody = `{"data":{"data":{"other":"value"},"metadata":{"version":1,"created_time":"2024-01-01T00:00:00Z","deletion_time":"","destroyed":false}}}`
	c := newTestClient(t, srv.srv.URL)
	_, err := c.Read(t.Context(), "iam/serviceaccount/read")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Contains(t, de.Message, `missing "secret"`)
}

func TestRead_KVFailure(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.readStatus = http.StatusInternalServerError
	srv.readBody = `{"errors":["boom"]}`
	c := newTestClient(t, srv.srv.URL)
	_, err := c.Read(t.Context(), "iam/serviceaccount/read")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Contains(t, de.Message, "read_secret")
}

func TestRead_LoginFailurePropagates(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.loginStatus = http.StatusInternalServerError
	srv.loginBody = `{"errors":["down"]}`
	c := newTestClient(t, srv.srv.URL)
	_, err := c.Read(t.Context(), "iam/serviceaccount/read")
	require.Error(t, err)
}

func TestDelete_Success(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)
	require.NoError(t, c.Delete(t.Context(), "iam/serviceaccount/delete"))
}

func TestDelete_NotFoundIsNoop(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.deleteStatus = http.StatusNotFound
	srv.deleteBody = "{}"
	c := newTestClient(t, srv.srv.URL)
	require.NoError(t, c.Delete(t.Context(), "iam/serviceaccount/delete"))
}

func TestDelete_KVFailure(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.deleteStatus = http.StatusInternalServerError
	srv.deleteBody = `{"errors":["boom"]}`
	c := newTestClient(t, srv.srv.URL)
	err := c.Delete(t.Context(), "iam/serviceaccount/delete")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Contains(t, de.Message, "delete_secret")
}

func TestDelete_LoginFailurePropagates(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.loginStatus = http.StatusInternalServerError
	srv.loginBody = `{"errors":["down"]}`
	c := newTestClient(t, srv.srv.URL)
	err := c.Delete(t.Context(), "iam/serviceaccount/delete")
	require.Error(t, err)
}

func TestList_Success(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)
	keys, err := c.List(t.Context(), "iam/serviceaccount/list")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1", "v2"}, keys)
}

func TestList_NotFoundReturnsNilNotError(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.listStatus = http.StatusNotFound
	srv.listBody = "{}"
	c := newTestClient(t, srv.srv.URL)
	keys, err := c.List(t.Context(), "iam/serviceaccount/list")
	require.NoError(t, err)
	assert.Nil(t, keys)
}

func TestList_KVFailure(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.listStatus = http.StatusInternalServerError
	srv.listBody = `{"errors":["boom"]}`
	c := newTestClient(t, srv.srv.URL)
	_, err := c.List(t.Context(), "iam/serviceaccount/list")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Contains(t, de.Message, "list_secrets")
}

func TestList_LoginFailurePropagates(t *testing.T) {
	srv := newFakeBaoServer(t)
	srv.loginStatus = http.StatusInternalServerError
	srv.loginBody = `{"errors":["down"]}`
	c := newTestClient(t, srv.srv.URL)
	_, err := c.List(t.Context(), "iam/serviceaccount/list")
	require.Error(t, err)
}

// sanity: confirm our fake login response actually round-trips the shape
// the SDK expects, catching a JSON-shape typo in the fixtures above before
// it masquerades as a client.go bug.
// TestClient_ConcurrentCallsDoNotRaceOnSharedToken is a regression test for
// a real data race: kvClient used to call c.baoClient.SetToken(tok)
// directly on the single shared *openbaoapi.Client, then hand the caller a
// KVv2 handle to use afterward with no lock held — under concurrent
// requests (the normal case for cmd/server), one goroutine's SetToken could
// be clobbered by another's before its own KV call executed. kvClient now
// clones the underlying client per call (sharing the connection pool, not
// the token field) specifically to remove this race. Run with -race; this
// test only proves absence of a race, not any particular call ordering.
func TestClient_ConcurrentCallsDoNotRaceOnSharedToken(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 4)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = c.Write(t.Context(), "iam/serviceaccount/write", "s3cr3t")
		}()
		go func() {
			defer wg.Done()
			_, _ = c.Read(t.Context(), "iam/serviceaccount/read")
		}()
		go func() {
			defer wg.Done()
			_ = c.Delete(t.Context(), "iam/serviceaccount/delete")
		}()
		go func() {
			defer wg.Done()
			_, _ = c.List(t.Context(), "iam/serviceaccount/list")
		}()
	}
	wg.Wait()
}

func TestFakeBaoServer_LoginBodyIsWellFormed(t *testing.T) {
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"auth":{"client_token":"fake-token","lease_duration":3600}}`), &v))
	auth, ok := v["auth"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fake-token", auth["client_token"])
	require.Equal(t, fmt.Sprintf("%v", float64(3600)), fmt.Sprintf("%v", auth["lease_duration"]))
}

func TestInstrumentedHTTPClient_WrapsTransportWithOTel(t *testing.T) {
	c := instrumentedHTTPClient(3*time.Second, nil)
	require.NotNil(t, c)
	assert.Equal(t, 3*time.Second, c.Timeout)
	_, ok := c.Transport.(*otelhttp.Transport)
	assert.True(t, ok, "OpenBao HTTP transport must be otelhttp so calls join gincommon traces")

	base := &http.Client{Timeout: time.Second}
	wrapped := instrumentedHTTPClient(5*time.Second, base)
	assert.Equal(t, 5*time.Second, wrapped.Timeout)
	_, ok = wrapped.Transport.(*otelhttp.Transport)
	assert.True(t, ok)
}
