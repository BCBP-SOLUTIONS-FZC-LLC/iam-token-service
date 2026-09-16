// Package openbao implements port.SecretStore against OpenBao's KV v2 API
// using the official openbao/api/v2 SDK (§3.1). Auth is via OpenBao's
// Kubernetes auth method (pod ServiceAccount token) — never a static token
// and never AWS IAM (§10.5, TS-CONFIG-3).
package openbao

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	openbaoapi "github.com/openbao/openbao/api/v2"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// secretDataKey is the single field name under which this service stores
// credential plaintext in a KV v2 entry — an implementation detail never
// surfaced to a caller (TS-INV-2).
const secretDataKey = "secret"

// Config configures the OpenBao KV v2 client (§12).
type Config struct {
	Addr                string // OPENBAO_ADDR
	AuthRole            string // OPENBAO_ROLE
	KVMount             string // OPENBAO_KV_MOUNT ("iam")
	KubernetesTokenPath string // pod ServiceAccount token; defaults to the projected volume path
	HTTPTimeout         time.Duration
}

// Client implements port.SecretStore.
type Client struct {
	cfg Config
	log port.Logger

	mu          sync.Mutex
	baoClient   *openbaoapi.Client
	cachedToken string
	expiresAt   time.Time
}

var _ port.SecretStore = (*Client)(nil)

// New constructs a Client using cfg, defaulting the Kubernetes token path
// and HTTP timeout when unset.
func New(cfg Config, log port.Logger) (*Client, error) {
	if cfg.KubernetesTokenPath == "" {
		cfg.KubernetesTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	if cfg.HTTPTimeout == 0 {
		cfg.HTTPTimeout = 8 * time.Second
	}
	baoCfg := openbaoapi.DefaultConfig()
	baoCfg.Address = cfg.Addr
	baoCfg.Timeout = cfg.HTTPTimeout
	// Wrap the SDK's HTTP client so OpenBao KV/auth calls inject W3C
	// traceparent and emit client spans on gincommon's TracerProvider —
	// same outbound pattern as iam-org-membership's httpx package.
	baoCfg.HttpClient = instrumentedHTTPClient(cfg.HTTPTimeout, baoCfg.HttpClient)
	baoClient, err := openbaoapi.NewClient(baoCfg)
	if err != nil {
		return nil, fmt.Errorf("openbao: new client: %w", err)
	}
	return &Client{cfg: cfg, log: log, baoClient: baoClient}, nil
}

// Health reports OpenBao reachability for /readyz (§13.4, §14.4 smoke) by
// exercising the same Kubernetes-auth login path every real request
// depends on — a cached, unexpired token short-circuits without a network
// call.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.token(ctx)
	return err
}

// Write puts secret at the deterministic KV v2 path (§6.3). Returns
// domain.ErrSecretStoreUnavailable on failure.
func (c *Client) Write(ctx context.Context, path string, secret string) error {
	kv, err := c.kvClient(ctx)
	if err != nil {
		return err
	}
	if _, err := kv.Put(ctx, c.mountRelativePath(path), map[string]any{secretDataKey: secret}); err != nil {
		return wrapErr("write_secret", err)
	}
	return nil
}

// Read returns the plaintext at path — used only by TS-1's
// rotation_id-replay path (§9.2). Returns domain.ErrSecretStoreUnavailable
// on failure or a missing entry.
func (c *Client) Read(ctx context.Context, path string) (string, error) {
	kv, err := c.kvClient(ctx)
	if err != nil {
		return "", err
	}
	secret, err := kv.Get(ctx, c.mountRelativePath(path))
	if err != nil {
		return "", wrapErr("read_secret", err)
	}
	if secret == nil || secret.Data == nil {
		return "", wrapErr("read_secret", fmt.Errorf("no secret at path"))
	}
	v, ok := secret.Data[secretDataKey].(string)
	if !ok {
		return "", wrapErr("read_secret", fmt.Errorf("secret data missing %q field", secretDataKey))
	}
	return v, nil
}

// Delete permanently removes all versions and metadata at path (CUST-2 —
// a full KV v2 metadata delete, not a soft/recoverable delete). A missing
// path is a no-op, not an error, so revoke/offboarding/the §8.6
// reconciler stay idempotent.
func (c *Client) Delete(ctx context.Context, path string) error {
	kv, err := c.kvClient(ctx)
	if err != nil {
		return err
	}
	if err := kv.DeleteMetadata(ctx, c.mountRelativePath(path)); err != nil {
		if isNotFound(err) {
			return nil
		}
		return wrapErr("delete_secret", err)
	}
	return nil
}

// List returns the immediate child path segments under pathPrefix (KV v2
// LIST) — used by the §8.6 orphan-material reconciler.
func (c *Client) List(ctx context.Context, pathPrefix string) ([]string, error) {
	kv, err := c.kvClient(ctx)
	if err != nil {
		return nil, err
	}
	list, err := kv.List(ctx, c.mountRelativePath(pathPrefix))
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, wrapErr("list_secrets", err)
	}
	if list == nil {
		return nil, nil
	}
	return list.Keys, nil
}

// mountRelativePath strips the KV mount prefix from path before handing it
// to the KVv2 SDK. domain.OpenBaoPathFor (and the §8.6 reconciler's prefix
// construction) produce the FULL external path per the frozen §25 spec —
// e.g. "iam/serviceaccount/<tenant>/<client>/v<n>", matching the value
// stored in Postgres and returned via the API — but every KVv2 method
// (Put/Get/List/DeleteMetadata) independently re-prepends the mount itself
// (kv.mountPath + "/data|metadata/" + secretPath). Passing the full path
// unstripped would silently double the mount segment
// ("iam/data/iam/serviceaccount/...") — self-consistent between
// Write/Read/Delete/List (all three make the same mistake, so round-trips
// still "work"), but mismatched against any real OpenBao ACL policy scoped
// to the correct mount-relative path (deploy/openbao/policy.hcl).
func (c *Client) mountRelativePath(path string) string {
	return strings.TrimPrefix(path, c.cfg.KVMount+"/")
}

// kvClient returns the KV v2 handle for the configured mount, authorized
// with a freshly-logged-in or cached token (§10.5).
//
// Clone (not c.baoClient.SetToken directly) is deliberate: this service's
// Client is shared across concurrent requests (cmd/server handles them
// concurrently), and *openbaoapi.Client.SetToken mutates a plain field on
// the shared client with no locking of its own — two goroutines calling
// kvClient at once could interleave their SetToken calls before either's
// actual KV request executes, so one request could authenticate (or fail
// to authenticate) as the wrong caller's token. Clone() creates a new
// *openbaoapi.Client wrapper with its own independent token field while
// reusing the same underlying http.Client (and its connection pool), so
// this costs no new TCP/TLS handshake — only a small struct allocation —
// and removes the race entirely.
func (c *Client) kvClient(ctx context.Context) (*openbaoapi.KVv2, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	cloned, err := c.baoClient.Clone()
	if err != nil {
		return nil, wrapErr("clone_client", err)
	}
	cloned.SetToken(tok)
	return cloned.KVv2(c.cfg.KVMount), nil
}

// token returns a cached OpenBao client token, logging in via the
// Kubernetes auth method when absent or expired.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedToken != "" && time.Now().Before(c.expiresAt) {
		return c.cachedToken, nil
	}
	jwt, err := os.ReadFile(c.cfg.KubernetesTokenPath)
	if err != nil {
		// A missing/unreadable pod ServiceAccount token means this service
		// cannot authenticate to OpenBao — functionally the same
		// "OpenBao unavailable" outcome as a failed login or a failed KV
		// call (§5.4 TS-1: 502 secret_store_unavailable), not an
		// unclassified 500. Must be wrapped here, not left as a bare
		// fmt.Errorf: unwrapped, this was the one path in the client that
		// bypassed domain-error classification and fell through to the
		// HTTP layer's generic 500 internal_error handler.
		return "", wrapErr("read_kubernetes_token", err)
	}
	secret, err := c.baoClient.Logical().WriteWithContext(ctx, "auth/kubernetes/login", map[string]any{
		"role": c.cfg.AuthRole,
		"jwt":  strings.TrimSpace(string(jwt)),
	})
	if err != nil {
		return "", wrapErr("kubernetes_login", err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return "", wrapErr("kubernetes_login", fmt.Errorf("empty auth response"))
	}
	c.cachedToken = secret.Auth.ClientToken
	// 30s safety margin against clock skew / request latency.
	c.expiresAt = time.Now().Add(time.Duration(secret.Auth.LeaseDuration)*time.Second - 30*time.Second)
	return c.cachedToken, nil
}

// instrumentedHTTPClient wraps base (DefaultConfig's pooled client, or a
// fresh client when nil) with otelhttp so every OpenBao round-trip joins
// the request/job span installed by gincommon.InitTracingFromEnv.
func instrumentedHTTPClient(timeout time.Duration, base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	if timeout > 0 {
		base.Timeout = timeout
	}
	base.Transport = otelhttp.NewTransport(base.Transport)
	return base
}

// wrapErr maps any OpenBao SDK failure to domain.ErrSecretStoreUnavailable
// (§5.4 TS-1 502, §17) while keeping op and the underlying error in the
// message for diagnostics — never a secret value (TS-INV-2).
func wrapErr(op string, err error) *domain.Error {
	return domain.NewError(domain.ErrSecretStoreUnavailable, fmt.Sprintf("openbao: %s: %v", op, err))
}

// isNotFound reports whether err represents OpenBao's 404 (no secret at
// this path) — an *openbaoapi.ResponseError with StatusCode 404.
func isNotFound(err error) bool {
	var respErr *openbaoapi.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == 404
	}
	return false
}
