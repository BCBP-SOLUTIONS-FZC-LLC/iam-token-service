//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

const appRolePassword = "apppassword-testonly"

// setupPostgres starts a minimal, single-role Postgres testcontainer for
// this package's own use — a trimmed duplicate of
// test/postgres/rls_test.go's setupTestDB (not importable across packages/
// build-tag combinations): only serviceaccount_app is needed here, since
// this test drives the real service layer through its normal RLS-scoped
// path, not the reconciler's BYPASSRLS enumeration.
func setupPostgres(t *testing.T) *pgcommon.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping postgres integration test in short mode")
	}
	ctx := context.Background()

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
				break
			}
			_ = pgContainer.Terminate(ctx)
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	require.NoError(t, err, "postgres testcontainer failed after %d attempts", maxAttempts)
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	rawPool, err := dbseed.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE serviceaccount_app LOGIN PASSWORD '%s' NOBYPASSRLS`, appRolePassword))
	require.NoError(t, err)

	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: superDSN}))
	require.NoError(t, pgadapter.RunMigrations(ctx, superDSN))

	appDSN := strings.Replace(superDSN, "postgres:testpassword@", "serviceaccount_app:"+appRolePassword+"@", 1)
	appPool, err := pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           appDSN,
		PGBouncerMode: false,
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	return appPool
}

// withTenant returns a context carrying a pgcommon GUCSet so the pool's
// GUCProvider emits `SET LOCAL app.tenant_id = <uuid>` on every checkout
// (RLS-6).
func withTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// TestCredentialLifecycle_RealPostgresAndOpenBao is the one test in the
// whole suite proving Postgres and OpenBao genuinely agree with each other
// in production-shaped code, not through any fake on either side: register
// a principal -> issue a credential (secret actually retrievable from the
// real OpenBao container at the exact domain.OpenBaoPathFor path) ->
// rotate (prior material still present until the sweep, new material
// written) -> revoke (OpenBao material actually gone from the real
// container).
func TestCredentialLifecycle_RealPostgresAndOpenBao(t *testing.T) {
	t.Parallel()
	appPool := setupPostgres(t)
	baoEnv := setupOpenBao(t)
	baoClient := newClient(baoEnv)
	ctx := context.Background()

	principalRepo := pgadapter.NewPrincipalRepository(appPool)
	credentialRepo := pgadapter.NewCredentialRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil) // no outbox publisher needed — this test asserts Postgres+OpenBao agreement, not the event system

	principalSvc := service.NewPrincipalService(principalRepo, credentialRepo, txRunner)
	credentialSvc := service.NewCredentialService(principalRepo, credentialRepo, baoClient, txRunner, noopLogger{}, nil)

	tenantID := uuid.New()
	principalSub := uuid.New()
	actor := domain.SystemPrincipalID

	// Register.
	regCtx := withTenant(ctx, tenantID)
	regRes, err := principalSvc.Register(regCtx, tenantID, service.RegisterRequest{
		PrincipalSub: principalSub, KeycloakClientID: "platform-automation",
	}, actor)
	require.NoError(t, err)
	require.True(t, regRes.Created)
	principalID := regRes.PrincipalID

	// Issue v1.
	issueCtx := withTenant(ctx, tenantID)
	issueRes, err := credentialSvc.IssueOrRotate(issueCtx, tenantID, principalID, service.IssueOrRotateRequest{
		RotationID: uuid.New(), OverlapSeconds: 300,
	}, actor)
	require.NoError(t, err)
	assert.Equal(t, 1, issueRes.Version)
	require.False(t, issueRes.Replayed)

	// The secret must actually be retrievable from the REAL OpenBao
	// container at the exact frozen path (§6.3/§25) — not just returned in
	// the response.
	storedV1, err := baoClient.Read(context.Background(), issueRes.OpenBaoPath)
	require.NoError(t, err)
	assert.Equal(t, issueRes.Secret, storedV1)
	assert.Equal(t, domain.OpenBaoPathFor(tenantID, "platform-automation", 1), issueRes.OpenBaoPath)

	// Rotate to v2.
	rotateCtx := withTenant(ctx, tenantID)
	rotateRes, err := credentialSvc.IssueOrRotate(rotateCtx, tenantID, principalID, service.IssueOrRotateRequest{
		RotationID: uuid.New(), OverlapSeconds: 300,
	}, actor)
	require.NoError(t, err)
	assert.Equal(t, 2, rotateRes.Version)
	require.NotNil(t, rotateRes.ExpiresPriorAt)

	// v2's material is in OpenBao; v1's material is still there too (overlap
	// window has not expired) — both real, both independently verifiable.
	storedV2, err := baoClient.Read(context.Background(), rotateRes.OpenBaoPath)
	require.NoError(t, err)
	assert.Equal(t, rotateRes.Secret, storedV2)
	_, err = baoClient.Read(context.Background(), issueRes.OpenBaoPath)
	require.NoError(t, err, "v1's material must still be present during the overlap window")

	// Revoke v1 directly (TS-2) — its OpenBao material must actually be
	// gone from the real container afterward, not merely marked revoked in
	// Postgres.
	revokeCtx := withTenant(ctx, tenantID)
	revokeRes, err := credentialSvc.Revoke(revokeCtx, tenantID, principalID, 1, actor)
	require.NoError(t, err)
	assert.Equal(t, domain.CredentialStatusRevoked, revokeRes.Status)

	_, err = baoClient.Read(context.Background(), issueRes.OpenBaoPath)
	require.Error(t, err, "v1's OpenBao material must be deleted for real after revoke (CUST-2)")

	// v2 (the current active credential) is untouched.
	storedV2Again, err := baoClient.Read(context.Background(), rotateRes.OpenBaoPath)
	require.NoError(t, err)
	assert.Equal(t, rotateRes.Secret, storedV2Again)
}
