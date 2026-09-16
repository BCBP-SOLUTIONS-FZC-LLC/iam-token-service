//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	openbaoadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/openbao"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// These constants mirror deploy/openbao/policy.hcl and
// deploy/openbao/role.tf.example verbatim (real production ACL + role
// binding), and OPENBAO_ROLE/OPENBAO_KV_MOUNT from cmd/server/main.go's env
// wiring (§12) — the real production values, not invented test-only ones,
// so a drift between the real ACL/role and this test's fixture would have
// to be introduced in both places at once.
const (
	baoRoleName    = "iam-token-service"
	baoSANamespace = "iam"
	baoSAName      = "iam-token-service"
	baoKVMount     = "iam"
)

// baoPolicyHCL is deploy/openbao/policy.hcl's body, used verbatim.
const baoPolicyHCL = `
path "iam/data/serviceaccount/*" {
  capabilities = ["create", "read", "update"]
}

path "iam/metadata/serviceaccount/*" {
  capabilities = ["read", "delete", "list"]
}

path "auth/token/renew-self" {
  capabilities = ["update"]
}
`

var rootTokenRe = regexp.MustCompile(`Root Token: (\S+)`)

// noopLogger satisfies port.Logger with no-ops.
type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

var _ port.Logger = noopLogger{}

// openbaoEnv is everything a test needs to construct one or more
// openbao.Client instances against one real, fully-bootstrapped OpenBao
// container: KV v2 policy + Kubernetes auth (role bound to baoSAName/
// baoSANamespace) are already configured, and tokenPath already holds a
// real signed ServiceAccount JWT satisfying that role's bindings.
type openbaoEnv struct {
	addr      string
	rootToken string
	tokenPath string
	// loginCount counts real TokenReview calls the fake Kubernetes API
	// server received — the only place a Kubernetes-auth login is
	// observable from outside the OpenBao container, used to verify
	// Client.token()'s cache actually avoids re-authenticating on every
	// call (see TestOpenBao_TokenIsCachedAcrossCalls).
	loginCount *atomic.Int64
}

// newClient builds an openbao.Client against env using TS's real
// production Config shape (internal/adapter/outbound/openbao/client.go) —
// unlike a sibling service's OpenBao adapter, this Client takes the full
// KV path per-call (Write/Read/Delete/List all take `path` directly,
// mirroring domain.OpenBaoPathFor's output) rather than a configured path
// prefix, so there is no separate "scoped" constructor variant here.
func newClient(env *openbaoEnv) *openbaoadapter.Client {
	c, err := openbaoadapter.New(openbaoadapter.Config{
		Addr:                env.addr,
		AuthRole:            baoRoleName,
		KVMount:             baoKVMount,
		KubernetesTokenPath: env.tokenPath,
	}, noopLogger{})
	if err != nil {
		panic(err) // Config here is always well-formed; a failure is a test bug.
	}
	return c
}

// setupOpenBao starts a real OpenBao dev-mode testcontainer and fully
// bootstraps it against real Kubernetes-auth: no live Kubernetes cluster is
// ever contacted. internal/adapter/outbound/openbao/client.go always
// authenticates via OpenBao's Kubernetes auth method (§10.5, TS-CONFIG-3) —
// the standard way to exercise that for real without a cluster is OpenBao/
// Vault's pem_keys static-JWT-validation config
// (auth/kubernetes/config's "pem_keys" field, meant to let the plugin
// verify a ServiceAccount JWT's RS256 signature locally and skip the
// TokenReview API call entirely).
//
// That does NOT work against openbao/openbao:latest (confirmed empirically
// against this same image by the sibling Realm Provisioner's identical
// helper — see its test/integration/openbao_helpers_test.go doc comment):
// the login handler unconditionally calls POST
// .../apis/authentication.k8s.io/v1/tokenreviews regardless of whether
// pem_keys is configured; only the JWT *parsing* (namespace/name/uid
// extraction for role-matching) happens locally. So this helper instead
// runs a real TokenReview flow: a tiny in-test HTTP server plays the part
// of the Kubernetes API server's TokenReview endpoint (always answering
// "authenticated: true" for the one ServiceAccount identity baked into our
// hand-signed JWT), exposed to the OpenBao container via testcontainers'
// HostAccessPorts field (an SSHD-sidecar tunnel reachable at
// testcontainers.HostInternal — portable to Linux CI, unlike Docker
// Desktop's host.docker.internal). OpenBao's own kubernetes-auth code —
// real bound-service-account-name/namespace role matching, real JWT claim
// parsing, real token issuance — runs exactly as it would against a live
// cluster; only the cluster's TokenReview response is stubbed.
func setupOpenBao(t *testing.T) *openbaoEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping openbao integration test in short mode")
	}
	ctx := context.Background()

	priv := genRSAKeypair(t)
	saUID := "11111111-1111-1111-1111-111111111111"
	jwt := mintServiceAccountJWT(t, priv, baoSANamespace, baoSAName, saUID)
	tokenPath := writeSAToken(t, jwt)

	trPort, loginCount := startFakeTokenReviewServer(t, baoSANamespace, baoSAName, saUID)

	req := testcontainers.ContainerRequest{
		Image:           "openbao/openbao:latest",
		ExposedPorts:    []string{"8200/tcp"},
		Cmd:             []string{"server", "-dev"},
		HostAccessPorts: []int{trPort},
		WaitingFor:      wait.ForLog("Development mode should NOT be used in production installations!").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	mappedPort, err := container.MappedPort(ctx, "8200/tcp")
	require.NoError(t, err)
	host, err := container.Host(ctx)
	require.NoError(t, err)
	addr := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())

	logsReader, err := container.Logs(ctx)
	require.NoError(t, err)
	logs, err := io.ReadAll(logsReader)
	require.NoError(t, err)
	m := rootTokenRe.FindSubmatch(logs)
	require.NotNil(t, m, "root token not found in openbao startup logs:\n%s", string(logs))
	rootToken := string(m[1])

	// KV v2 is NOT mounted at "iam/" by default (unlike dev mode's default
	// "secret/" mount) — this service's own frozen mount name (§25,
	// OPENBAO_KV_MOUNT) must be created explicitly.
	baoDo(t, http.MethodPost, addr, rootToken, "/v1/sys/mounts/"+baoKVMount,
		map[string]any{"type": "kv", "options": map[string]any{"version": "2"}}, nil)

	// Write the real production policy verbatim.
	baoDo(t, http.MethodPut, addr, rootToken, "/v1/sys/policies/acl/"+baoRoleName,
		map[string]any{"policy": baoPolicyHCL}, nil)

	// Enable Kubernetes auth.
	baoDo(t, http.MethodPost, addr, rootToken, "/v1/sys/auth/kubernetes",
		map[string]any{"type": "kubernetes"}, nil)

	// Configure it against the fake TokenReview stub. kubernetes_ca_cert
	// must be a well-formed PEM even though it is never actually used to
	// dial TLS here (the stub is plain HTTP) — OpenBao errors out at
	// config-write time if it's empty and no local in-cluster CA file
	// exists.
	baoDo(t, http.MethodPost, addr, rootToken, "/v1/auth/kubernetes/config",
		map[string]any{
			"kubernetes_host":        fmt.Sprintf("http://%s:%d", testcontainers.HostInternal, trPort),
			"kubernetes_ca_cert":     generateDummyCACertPEM(t),
			"disable_iss_validation": true,
		}, nil)

	// Role bound to the same ServiceAccount identity baked into our JWT —
	// deploy/openbao/role.tf.example's real binding, verbatim.
	baoDo(t, http.MethodPost, addr, rootToken, "/v1/auth/kubernetes/role/"+baoRoleName,
		map[string]any{
			"bound_service_account_names":      []string{baoSAName},
			"bound_service_account_namespaces": []string{baoSANamespace},
			"policies":                         []string{baoRoleName},
			"ttl":                              "1h",
		}, nil)

	return &openbaoEnv{addr: addr, rootToken: rootToken, tokenPath: tokenPath, loginCount: loginCount}
}

func genRSAKeypair(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return priv
}

// mintServiceAccountJWT hand-signs a realistic (legacy-format, pre-Kubernetes
// 1.21 "flat claim") ServiceAccount JWT: iss/sub plus the
// "kubernetes.io/serviceaccount/*" claim set OpenBao's kubernetes-auth
// plugin parses to identify the caller and match it against a role's
// bound_service_account_names/namespaces.
func mintServiceAccountJWT(t *testing.T, priv *rsa.PrivateKey, namespace, saName, uid string) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT"}
	now := time.Now()
	claims := map[string]any{
		"iss":                                    "kubernetes/serviceaccount",
		"sub":                                    fmt.Sprintf("system:serviceaccount:%s:%s", namespace, saName),
		"kubernetes.io/serviceaccount/namespace": namespace,
		"kubernetes.io/serviceaccount/service-account.name": saName,
		"kubernetes.io/serviceaccount/service-account.uid":  uid,
		"kubernetes.io/serviceaccount/secret.name":          saName + "-token",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
	hb, err := json.Marshal(header)
	require.NoError(t, err)
	cb, err := json.Marshal(claims)
	require.NoError(t, err)
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	require.NoError(t, err)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// writeSAToken writes jwt to a temp file, returning its path — this becomes
// Config.KubernetesTokenPath, standing in for the projected ServiceAccount
// token volume Client.token() reads in a real pod.
func writeSAToken(t *testing.T, jwt string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(path, []byte(jwt), 0o600))
	return path
}

// startFakeTokenReviewServer plays the part of the Kubernetes API server's
// TokenReview endpoint (see setupOpenBao's doc comment): it always answers
// "authenticated: true" for the single ServiceAccount identity this test
// package's JWT is signed for. Returns the port to reach it at plus a
// counter of TokenReview requests received (OpenBao's config-write-time
// bootstrap calls in setupOpenBao do NOT hit this endpoint, only real
// kubernetes-auth logins do, so the counter starts at 0 before any
// Client.token() call).
func startFakeTokenReviewServer(t *testing.T, namespace, saName, uid string) (int, *atomic.Int64) {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		count.Add(1)
		resp := map[string]any{
			"apiVersion": "authentication.k8s.io/v1",
			"kind":       "TokenReview",
			"status": map[string]any{
				"authenticated": true,
				"user": map[string]any{
					"username": fmt.Sprintf("system:serviceaccount:%s:%s", namespace, saName),
					"uid":      uid,
					"groups":   []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace, "system:authenticated"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err)
	return port, &count
}

// generateDummyCACertPEM returns a throwaway self-signed certificate PEM —
// OpenBao's auth/kubernetes/config requires a non-empty kubernetes_ca_cert
// even though the fake TokenReview server in this test suite is plain HTTP
// and never actually dials TLS with it.
func generateDummyCACertPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "openbao-integration-test-fake-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// ── root-token-authenticated raw HTTP helpers ───────────────────────────
//
// These bypass the adapter under test entirely — used both for real-server
// bootstrap (mount/policy/auth/role setup, no Vault-API-compatible Go SDK
// is vendored in this repo) and as an independent verification channel in
// test assertions.

// baoDo issues a root-token-authenticated request against addr+path and, if
// out is non-nil, JSON-decodes a 2xx response body into it. It fails the
// test on any non-2xx status.
func baoDo(t *testing.T, method, addr, token, path string, body any, out any) {
	t.Helper()
	resp := baoDoRaw(t, method, addr, token, path, body)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Truef(t, resp.StatusCode < 300, "%s %s: unexpected status %d: %s", method, path, resp.StatusCode, string(b))
	if out != nil && len(b) > 0 {
		require.NoError(t, json.Unmarshal(b, out))
	}
}

func baoDoRaw(t *testing.T, method, addr, token, path string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, addr+path, reader)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}
