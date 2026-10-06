//go:build e2e

package e2e_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestOversizedTenantRolesHeaderIsIgnored (TS-D23): this service never reads
// x-tenant-roles, so a header gincommon would reject (over its 4096-byte
// cap) must not turn a valid system-principal request into a 401.
func TestOversizedTenantRolesHeaderIsIgnored(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	registerPrincipal(t, tenantID)

	headers := sysHeaders(tenantID)
	headers["x-tenant-roles"] = strings.Repeat("role,", 2000) // 10 KB, 2000 roles
	resp, body := doRequest(t, http.MethodGet,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/platform-automation", headers, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
}
