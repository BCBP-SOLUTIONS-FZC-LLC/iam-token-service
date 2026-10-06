// Package realmprovisioner implements port.RealmProvisionerClient — the
// outbound HTTP call cmd/scheduler makes to the sibling Realm
// Provisioner's RP-17 endpoint (§16 TSQ-6 Resolved, TS-D14) to refresh
// Keycloak's cached JWKS after a rotation. This is the one outbound HTTP
// dependency this service has on another IAM service; it never calls
// Keycloak itself (TS-INV-1) and — under the EXT-6 client-jwt/JWKS
// mechanism (rev 1.3) — it passes RP no key material at all: RP triggers
// Keycloak's clear-keys-cache, and Keycloak fetches the public key from
// this service's own JWKS.
//
// Mirrors the Realm Provisioner's own internal/adapter/outbound/tokenservice
// client (its outbound call into this service's TS-1/TS-3/TS-4) — same
// system-principal header pattern, same status-code classification shape.
package realmprovisioner

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/httpx"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// Client implements port.RealmProvisionerClient against the Realm
// Provisioner's internal API.
type Client struct {
	baseURL string
	http    *http.Client
	log     port.Logger
}

var _ port.RealmProvisionerClient = (*Client)(nil)

// requestTimeout is deliberately shorter than the shared 5s httpx default
// used elsewhere (production-readiness review, TS-D15) — RP is an in-mesh
// service, not a cross-region dependency, and this timeout multiplies
// directly into RefreshKeys' worst-case per-row cost (see retryBackoff's
// doc comment below); 5s here would make that arithmetic not close against
// the CronJob's activeDeadlineSeconds budget.
const requestTimeout = 3 * time.Second

// New constructs a Client calling the Realm Provisioner's internal API at
// baseURL (REALM_PROVISIONER_BASE_URL, §12).
func New(baseURL string, log port.Logger) *Client {
	return &Client{baseURL: baseURL, http: httpx.NewClient(requestTimeout), log: log}
}

// retryBackoff is the delay before the 2nd of RefreshKeys' 2 total attempts.
// Kept to exactly one retry, and requestTimeout kept short (above), because
// this budget multiplies directly against how many due/expired rows
// cmd/rotator's sweep and cmd/scheduler's scan can process inside one
// unattended CronJob run: worst case (RP down, every row retries once) is
// 2×requestTimeout + this backoff ≈ 6.3s per row, not the ~16s/row a naive
// 3-attempt/5s-timeout policy would cost — the difference between ~35 due
// rows safely fitting in the 240s activeDeadlineSeconds default and only
// ~15 (production-readiness review, TS-D15; see also
// ROTATOR_RUN_TIMEOUT/SCHEDULER_RUN_TIMEOUT's own default, tuned in the
// same review to stay safely under that same k8s deadline so the graceful
// in-process exit path always wins the race against a hard SIGKILL).
var retryBackoff = []time.Duration{300 * time.Millisecond}

// RefreshKeys calls RP-17
// POST /api/v1/internal/tenants/:id/service-account/keys/refresh (empty
// body) so Keycloak re-fetches the tenant's platform-automation JWKS after
// a rotation — the newly-issued key becomes recognized and a removed one
// forgotten. No key material is sent: under the EXT-6 client-jwt/JWKS
// mechanism RP triggers Keycloak's clear-keys-cache rather than receiving
// a secret (rev 1.3; replaces the removed ApplySecret / credentials-apply
// call).
//
// Retries once (2 attempts total, see retryBackoff) on a plausibly-transient
// failure — RP-17 is documented idempotent (clearing an already-fresh cache
// is a no-op, RP LLD §2.5), and a network-level failure, RP-17's own
// documented 502 (keycloak_unavailable) and a mesh-level 429/503/504 qualify; retrying here absorbs
// the kind of blip that would otherwise page on-call for nothing
// (cmd/scheduler's cadence_rotation_total{result="failed"}, cmd/rotator's
// rotation_sweep_total{result="error"}) every 5-minute CronJob tick. A 422
// (service_account_not_provisioned) or any other unexpected status is a
// permanent business error — retrying cannot fix it, so it fails fast on
// the first attempt. Every failure is surfaced as a plain error, not a
// domain.ErrorCode: this service's own §17 taxonomy classifies errors an
// HTTP caller of THIS service sees; an RP-17 outcome never reaches one —
// cmd/rotator's and cmd/scheduler's callers classify retryable-vs-not by
// inspecting the returned error text, not a typed code.
func (c *Client) RefreshKeys(ctx context.Context, tenantID uuid.UUID) error {
	var lastErr error
	for attempt := 0; attempt < len(retryBackoff)+1; attempt++ {
		retryable, err := c.refreshKeysOnce(ctx, tenantID)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable || attempt == len(retryBackoff) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryBackoff[attempt]):
		}
	}
	return lastErr
}

// refreshKeysOnce makes a single RP-17 attempt. retryable distinguishes a
// plausibly-transient failure (unreachable; RP-17's own documented 502
// keycloak_unavailable) from a permanent one (any other status, including
// 422 service_account_not_provisioned) that a retry cannot fix.
func (c *Client) refreshKeysOnce(ctx context.Context, tenantID uuid.UUID) (retryable bool, err error) {
	url := fmt.Sprintf("%s/api/v1/internal/tenants/%s/service-account/keys/refresh", c.baseURL, tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return false, err
	}
	// The reserved iam-system caller identity every IAM internal route
	// requires (§5.2/§10.4) — one constant for the whole service, so it can
	// never drift from what this service itself accepts.
	req.Header.Set("x-user-id", domain.SystemPrincipalID.String())
	req.Header.Set("x-tenant-id", tenantID.String())

	resp, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("realmprovisioner: RP-17 unreachable: %w", err)
	}
	defer closeBody(c.log, resp.Body)

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return false, nil // RP-17 answers 204; any 2xx is success
	case resp.StatusCode == http.StatusBadGateway:
		return true, fmt.Errorf("realmprovisioner: RP-17 reports keycloak_unavailable (502)")
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusGatewayTimeout:
		// Mesh/proxy-level transients (an RP restart, "no healthy upstream",
		// rate limiting): worth the single retry. A post-commit RP-17 failure
		// does self-heal through keys_refresh_pending (TS-D22), but only on a
		// later CronJob run (≥4 minutes), and it still counts as failed and
		// pages — so absorbing a blip here is still worth it.
		return true, fmt.Errorf("realmprovisioner: RP-17 temporarily unavailable (%d)", resp.StatusCode)
	default:
		return false, fmt.Errorf("realmprovisioner: unexpected status %d from RP-17", resp.StatusCode)
	}
}

// closeBody drains (bounded) and closes body, logging (not propagating) a
// failure. Draining lets the transport reuse the keep-alive connection.
func closeBody(log port.Logger, body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	if err := body.Close(); err != nil && log != nil {
		log.Warn("realmprovisioner: close response body failed", map[string]interface{}{"error": err.Error()})
	}
}
