//go:build e2e

// Infra + middleware tests — modeled on the sibling iam-org-membership
// service's test/e2e/infra_test.go. Verifies:
//
//   - /healthz, /readyz are reachable without any gateway identity headers
//     (LB probes). /metrics is served on its own dedicated port
//     (METRICS_PORT) — not on this router — so it isn't exercised here.
//   - X-Request-ID is echoed verbatim when the caller supplies one, and
//     generated when absent (platform-gincommon's RequestIDMiddleware).
//   - The 1MB request body cap (router.go's MaxBytesReader) is enforced.
//   - An empty (not just absent) x-user-id / x-tenant-id header is treated
//     as missing, not as some anonymous/zero identity.
//   - A well-formed but non-UUID x-tenant-id is rejected.
package e2e_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestHealthzUnauthenticated(t *testing.T) {
	t.Parallel()
	resp, body := doRequest(t, http.MethodGet, "/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("healthz body must not be empty")
	}
}

func TestReadyzWhenDBUp(t *testing.T) {
	t.Parallel()
	resp, body := doRequest(t, http.MethodGet, "/readyz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", resp.StatusCode, body)
	}
	if !bytes.Contains(body, []byte("ready")) {
		t.Fatalf("readyz body must mention readiness, got %s", body)
	}
}

func TestXRequestIDEchoedWhenProvided(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	custom := "req-e2e-" + uuid.NewString()

	headers := sysHeaders(tenantID)
	headers["X-Request-ID"] = custom

	resp, _ := doRequest(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+uuid.NewString(),
		headers, nil)
	if got := resp.Header.Get("X-Request-ID"); got != custom {
		t.Fatalf("X-Request-ID = %q, want %q (must be echoed verbatim when provided)", got, custom)
	}
}

func TestXRequestIDGeneratedWhenAbsent(t *testing.T) {
	t.Parallel()
	resp, _ := doRequest(t, http.MethodGet, "/healthz", nil, nil)
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("middleware must generate a request id when the client omits one")
	}
}

// TestBodyCapEnforced — a 2MB payload must be rejected; router.go's
// MaxBytesReader caps request bodies at 1MB.
func TestBodyCapEnforced(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	big := bytes.Repeat([]byte(`x`), 2<<20)
	resp, _ := doRequest(t, http.MethodPost, "/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts",
		sysHeaders(tenantID), big)
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want >= 400 for a 2MB body", resp.StatusCode)
	}
}

// TestEmptyUserIDHeaderRejected — x-user-id present but empty must be
// treated as missing (401), not as an anonymous/zero identity.
func TestEmptyUserIDHeaderRejected(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()

	resp, _ := doRequest(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+uuid.NewString(),
		map[string]string{
			"x-user-id":      "",
			"x-tenant-id":    tenantID.String(),
			"x-tenant-roles": "iam-system",
		}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an empty x-user-id", resp.StatusCode)
	}
}

// TestEmptyTenantIDHeaderRejected — x-tenant-id present but empty must be
// treated as missing.
func TestEmptyTenantIDHeaderRejected(t *testing.T) {
	t.Parallel()
	resp, _ := doRequest(t, http.MethodGet,
		"/api/v1/internal/tenants/"+uuid.NewString()+"/service-accounts/"+uuid.NewString(),
		map[string]string{
			"x-user-id":      "00000000-0000-0000-0000-0000000000a1",
			"x-tenant-id":    "",
			"x-tenant-roles": "iam-system",
		}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an empty x-tenant-id", resp.StatusCode)
	}
}

// TestNonUUIDTenantIDHeaderRejected — a well-formed but non-UUID
// x-tenant-id must be rejected by the GUCBridge middleware.
func TestNonUUIDTenantIDHeaderRejected(t *testing.T) {
	t.Parallel()
	resp, _ := doRequest(t, http.MethodGet,
		"/api/v1/internal/tenants/"+uuid.NewString()+"/service-accounts/"+uuid.NewString(),
		map[string]string{
			"x-user-id":      "00000000-0000-0000-0000-0000000000a1",
			"x-tenant-id":    "not-a-uuid",
			"x-tenant-roles": "iam-system",
		}, nil)
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want >= 400 for a non-UUID x-tenant-id", resp.StatusCode)
	}
}
