//go:build e2e

// Package e2e_test is the black-box end-to-end suite: real
// internal/adapter/inbound/http.Router (the exact NewRouter construction
// cmd/server/main.go/wiring.go build for production), fronting a real
// Postgres testcontainer, driven over real HTTP (httptest.NewServer). No
// fake stands in for Postgres — the RLS-6 invariant this suite exists to
// prove end-to-end. OpenBao is the only outbound dependency faked
// (in-memory, package-local): no scenario here needs a real KV v2 store,
// and standing one up would add container cost without adding HTTP-layer
// coverage (real OpenBao behavior is covered by test/integration
// instead). No AWS: the enqueue-time codec uses eventbus.NoopCodec exactly
// like production's dev/test fallback (schema-shape validation still
// happens via ValidatingCodec; only the Glue wire-encoding step is
// skipped), and no outbox.Runner publish loop is ever started — events are
// asserted as rows landing in outbox_events, never as anything actually
// published to SNS (mirrors the sibling Realm Provisioner service's e2e
// suite, which uses the identical strategy for its own AWS-shaped
// dependencies).
//
// TS-INV-1: this service has no Keycloak dependency at all, so unlike the
// sibling service's e2e suite there is no second heavyweight container to
// boot here.
//
// One Postgres container is started once for the whole package (TestMain)
// — every scenario mints its own fresh uuid-derived tenant/principal id,
// so nothing can collide across parallel tests sharing it.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/inbound/http"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ── package-level test environment, built once by TestMain ─────────────────

const appRolePassword = "e2e-app-password-testonly"

var (
	testServer *httptest.Server
	httpClient = &http.Client{Timeout: 30 * time.Second}

	rawPG *dbseed.Pool // pgcommon-backed superuser pool — bypasses RLS, used only for direct-state assertions
)

// noopLogger satisfies port.Logger with no-ops, keeping e2e output focused
// on test assertions rather than adapter debug/info noise (mirrors
// test/postgres's own pattern of not wiring a real logger).
type noopLogger struct{}

func (noopLogger) Debug(string, map[string]any) {}
func (noopLogger) Info(string, map[string]any)  {}
func (noopLogger) Warn(string, map[string]any)  {}
func (noopLogger) Error(string, map[string]any) {}

var _ port.Logger = noopLogger{}

// pingerFunc adapts a plain func to httpadapter.Pinger (mirrors
// cmd/server/main.go's identical helper).
type pingerFunc func(context.Context) error

func (f pingerFunc) Health(ctx context.Context) error { return f(ctx) }

// fakeSecretStore is a minimal in-memory port.SecretStore. Credential
// material never needs to survive a process restart in this suite (no
// DR/rebuild scenario is exercised here), so a real OpenBao container
// would add setup cost with no additional HTTP-layer coverage — TS-1/TS-2's
// HTTP-visible behavior does not depend on which SecretStore implementation
// backs it. Mirrors RP e2e's fakeOpenBao.
type fakeSecretStore struct {
	data map[string]string
}

func newFakeSecretStore() *fakeSecretStore { return &fakeSecretStore{data: map[string]string{}} }

func (f *fakeSecretStore) Write(_ context.Context, path, secret string) error {
	f.data[path] = secret
	return nil
}

func (f *fakeSecretStore) Read(_ context.Context, path string) (string, error) {
	v, ok := f.data[path]
	if !ok {
		return "", domain.NewError(domain.ErrSecretStoreUnavailable, "no secret at path")
	}
	return v, nil
}

func (f *fakeSecretStore) Delete(_ context.Context, path string) error {
	delete(f.data, path)
	return nil
}

func (f *fakeSecretStore) List(_ context.Context, pathPrefix string) ([]string, error) {
	var out []string
	for k := range f.data {
		if strings.HasPrefix(k, pathPrefix) {
			out = append(out, k)
		}
	}
	return out, nil
}

var _ port.SecretStore = (*fakeSecretStore)(nil)

func TestMain(m *testing.M) {
	code, err := runE2ESuite(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e setup failed:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// runE2ESuite builds the shared Postgres container and the
// production-wired router exactly once, runs the suite, and tears
// everything down — returning m.Run()'s exit code to TestMain.
func runE2ESuite(m *testing.M) (int, error) {
	ctx := context.Background()

	pgContainer, superDSN, err := startPostgresContainer(ctx)
	if err != nil {
		return 0, fmt.Errorf("start postgres container: %w", err)
	}
	defer func() { _ = pgContainer.Terminate(ctx) }()

	rawPG, err = dbseed.New(ctx, superDSN)
	if err != nil {
		return 0, fmt.Errorf("connect superuser pool: %w", err)
	}
	defer rawPG.Close()

	// Roles must exist BEFORE migrations run (mirrors test/postgres/rls_test.go's
	// setupTestDB ordering) — the domain migration's GRANT statements are
	// conditional on serviceaccount_app already existing.
	if _, err := rawPG.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE serviceaccount_app LOGIN PASSWORD '%s' NOBYPASSRLS`, appRolePassword)); err != nil {
		return 0, fmt.Errorf("create serviceaccount_app role: %w", err)
	}

	// outbox.ApplySchema FIRST (creates outbox_events with a JSONB payload
	// column) — the domain migration ALTERs that column to TEXT (§19).
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: superDSN}); err != nil {
		return 0, fmt.Errorf("apply outbox schema: %w", err)
	}
	if err := pgadapter.RunMigrations(ctx, superDSN); err != nil {
		return 0, fmt.Errorf("run domain migrations: %w", err)
	}

	appDSN := appRoleDSN(rawPG, appRolePassword)
	appPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           appDSN,
		PGBouncerMode: false, // testcontainer talks to Postgres directly
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	if err != nil {
		return 0, fmt.Errorf("connect app pool: %w", err)
	}
	defer appPool.Close()

	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-token-service-e2e", Logger: noopLogger{}})

	// ── Production wiring (mirrors cmd/server/main.go/wiring.go §6-9) ──────
	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	if err != nil {
		return 0, fmt.Errorf("build validating codec: %w", err)
	}
	outboxPublisher := eventbusadapter.New("iam-token-service", enqueueCodec).WithLogger(noopLogger{})
	txRunner := pgadapter.NewTxRunner(appPool, outboxPublisher)

	principalRepo := pgadapter.NewPrincipalRepository(appPool)
	credentialRepo := pgadapter.NewCredentialRepository(appPool)
	secrets := newFakeSecretStore()

	credentialSvc := service.NewCredentialService(principalRepo, credentialRepo, secrets, txRunner, noopLogger{}, nil)
	principalSvc := service.NewPrincipalService(principalRepo, credentialRepo, txRunner)

	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-token-service-e2e", Logger: noopLogger{}},
		Docs:      httpadapter.DocsConfig{Environment: "test"},
		Handlers: httpadapter.Handlers{
			Principal:  httpadapter.NewPrincipalHandler(principalSvc),
			Credential: httpadapter.NewCredentialHandler(credentialSvc),
		},
		Postgres: pingerFunc(func(ctx context.Context) error {
			if hs := appPool.Health(ctx); !hs.Healthy {
				return fmt.Errorf("database not healthy")
			}
			return nil
		}),
		OpenBao: pingerFunc(func(context.Context) error { return nil }),
		Outbox:  pingerFunc(func(context.Context) error { return nil }),
	})

	testServer = httptest.NewServer(router.Handler())
	defer testServer.Close()

	return m.Run(), nil
}

// startPostgresContainer mirrors test/postgres/rls_test.go's setupTestDB
// retry loop.
func startPostgresContainer(ctx context.Context) (*tcpostgres.PostgresContainer, string, error) {
	const maxAttempts = 3
	var pgContainer *tcpostgres.PostgresContainer
	var superDSN string
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		pgContainer, err = tcpostgres.Run(ctx,
			"postgres:16-alpine",
			tcpostgres.WithDatabase("serviceaccount"),
			tcpostgres.WithUsername("postgres"),
			tcpostgres.WithPassword("testpassword"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if err == nil {
			superDSN, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
			if err == nil {
				return pgContainer, superDSN, nil
			}
			_ = pgContainer.Terminate(ctx)
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	return nil, "", err
}

func appRoleDSN(rawPool *dbseed.Pool, password string) string {
	cfg := rawPool.Config().ConnConfig
	return fmt.Sprintf("postgres://serviceaccount_app:%s@%s:%d/%s?sslmode=disable",
		password, cfg.Host, cfg.Port, cfg.Database)
}

// ── HTTP helpers ─────────────────────────────────────────────────────────

// newTenantID returns a fresh, collision-free tenant id for one scenario.
func newTenantID() uuid.UUID { return uuid.New() }

// sysHeaders is a valid iam-system-principal caller scoped to tenantID —
// GUCBridgeMiddleware binds x-tenant-id straight into the RLS GUC (RLS-6),
// so every request acting on tenantID must carry it here, not an arbitrary
// id. x-user-id MUST be domain.SystemPrincipalID's literal string form —
// GUCBridgeMiddleware rejects any other value, even a syntactically valid
// UUID (§5.2's single authorization rule for this service).
func sysHeaders(tenantID uuid.UUID) map[string]string {
	return map[string]string{
		"x-user-id":      domain.SystemPrincipalID.String(),
		"x-tenant-id":    tenantID.String(),
		"x-tenant-roles": "iam-system",
		"Content-Type":   "application/json",
	}
}

// doRequest fires a real HTTP request at the shared httptest.Server and
// returns the raw response + body bytes.
func doRequest(t *testing.T, method, path string, headers map[string]string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, testServer.URL+path, rd)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, respBody
}

// doJSON marshals reqBody, fires the request, and decodes the response body
// into out (unless out is nil).
func doJSON(t *testing.T, method, path string, headers map[string]string, reqBody, out any) *http.Response {
	t.Helper()
	var raw []byte
	if reqBody != nil {
		var err error
		raw, err = json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
	}
	resp, respBody := doRequest(t, method, path, headers, raw)
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			t.Fatalf("decode response body: %v, status=%d, body=%s", err, resp.StatusCode, respBody)
		}
	}
	return resp
}

// decodeErrorBody decodes body as httpadapter.ErrorResponse for assertions
// on the §17 error shape.
func decodeErrorBody(t *testing.T, method, path string, headers map[string]string, reqBody any) (*http.Response, httpadapter.ErrorResponse) {
	t.Helper()
	var er httpadapter.ErrorResponse
	resp := doJSON(t, method, path, headers, reqBody, &er)
	return resp, er
}

func newRotationID() string { return uuid.NewString() }

// outboxEventCount returns how many outbox_events rows exist for
// (eventType, tenantID) — used to assert an event actually got enqueued
// (or, for the replay scenario, that no NEW row was added).
func outboxEventCount(t *testing.T, ctx context.Context, eventType string, tenantID uuid.UUID) int {
	t.Helper()
	var count int
	err := rawPG.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND tenant_id = $2`,
		eventType, tenantID.String(),
	).Scan(&count)
	if err != nil {
		t.Fatalf("query outbox_events: %v", err)
	}
	return count
}
