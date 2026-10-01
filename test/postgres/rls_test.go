//go:build integration

// Package postgres_test is the canonical Phase 1 test suite: spins up a
// fresh Postgres container via testcontainers-go, applies the outbox
// schema + the domain migration, and asserts the RLS invariants named in
// LLD §4.3/§4.4/§14.5 (RLS-1..RLS-7) plus the role-shape invariants
// (serviceaccount_app never holds BYPASSRLS; serviceaccount_reconciler
// holds BYPASSRLS but no write grant on either tenant-scoped table).
//
// Requires Docker on the runner. Tag: integration.
package postgres_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	appRolePassword       = "apppassword-testonly"
	migratorPassword      = "migratorpassword-testonly"
	reconcilerPassword    = "reconcilerpassword-testonly"
	adminReadonlyPassword = "adminreadonlypassword-testonly"
	testKeycloakClientID  = "platform-automation"
)

// setupTestDB spins up a Postgres container, creates the four roles the LLD
// calls out (§4.3/§4.4 — serviceaccount_app without BYPASSRLS,
// serviceaccount_migrator/serviceaccount_reconciler/admin_readonly with
// BYPASSRLS), applies outbox.ApplySchema then the domain migration (as
// superuser so DDL/CREATE ROLE grants succeed), and returns:
//
//	appPool        — pgcommon.Pool bound as serviceaccount_app, RLS
//	                 enforced, GUC-provider wired so `SET LOCAL
//	                 app.tenant_id` fires on every checkout (RLS-6).
//	reconcilerPool — pgcommon.Pool bound as serviceaccount_reconciler
//	                 (BYPASSRLS, SELECT-only), no GUC provider — mirrors
//	                 cmd/rotator's enumeration connection (§4.3/RLS-7).
//	rawPool        — dbseed.Pool (pgcommon-backed superuser) used only to
//	                 seed rows and assert catalog state.
func setupTestDB(t testing.TB) (appPool *pgcommon.Pool, reconcilerPool *pgcommon.Pool, rawPool *dbseed.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping postgres integration test in short mode")
	}
	ctx := context.Background()

	const maxContainerAttempts = 3
	var pgContainer *tcpostgres.PostgresContainer
	var superDSN string
	var err error
	for attempt := 1; attempt <= maxContainerAttempts; attempt++ {
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
		t.Logf("setupTestDB: postgres testcontainer attempt %d/%d failed: %v", attempt, maxContainerAttempts, err)
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	require.NoError(t, err, "postgres testcontainer failed after %d attempts", maxContainerAttempts)
	t.Cleanup(func() { _ = pgContainer.Terminate(ctx) })

	rawPool, err = dbseed.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)

	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE serviceaccount_app LOGIN PASSWORD '%s' NOBYPASSRLS`, appRolePassword))
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE serviceaccount_migrator LOGIN PASSWORD '%s' BYPASSRLS`, migratorPassword))
	require.NoError(t, err)
	// Production's serviceaccount_reconciler is NOLOGIN (§4.3 — cmd/rotator
	// connects via a short-lived credential/IAM-style mechanism, never a
	// static password); the test grants LOGIN so it can open a real
	// connection and exercise the role's actual grants.
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE serviceaccount_reconciler LOGIN PASSWORD '%s' BYPASSRLS`, reconcilerPassword))
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, fmt.Sprintf(
		`CREATE ROLE admin_readonly LOGIN PASSWORD '%s' BYPASSRLS`, adminReadonlyPassword))
	require.NoError(t, err)

	// outbox.ApplySchema FIRST (creates outbox_events with a JSONB payload
	// column) — the domain migration ALTERs that column to TEXT (MIG-2, §19).
	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: superDSN}))
	require.NoError(t, pgadapter.RunMigrations(ctx, superDSN))

	appDSN := strings.Replace(superDSN, "postgres:testpassword@", "serviceaccount_app:"+appRolePassword+"@", 1)
	appPool, err = pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           appDSN,
		PGBouncerMode: false, // testcontainer talks to Postgres directly
		GUCProvider:   pgcommon.GUCSetFromContext,
	})
	require.NoError(t, err)
	t.Cleanup(appPool.Close)

	reconcilerDSN := strings.Replace(superDSN, "postgres:testpassword@", "serviceaccount_reconciler:"+reconcilerPassword+"@", 1)
	reconcilerPool, err = pgcommon.NewPool(ctx, pgcommon.Config{
		DSN:           reconcilerDSN,
		PGBouncerMode: false,
		// No GUCProvider — the reconciler role enumerates cross-tenant via
		// BYPASSRLS, never via a tenant GUC (§4.3).
	})
	require.NoError(t, err)
	t.Cleanup(reconcilerPool.Close)

	return appPool, reconcilerPool, rawPool
}

// withTenant returns a context carrying a pgcommon GUCSet so the pool's
// GUCProvider emits `SET LOCAL app.tenant_id = <uuid>` on every checkout.
func withTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// seedPrincipal inserts a service_account_principals row via the superuser
// pool (bypasses RLS) and returns its id.
func seedPrincipal(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO service_account_principals (id, tenant_id, principal_sub, keycloak_client_id)
		VALUES ($1, $2, $3, $4)`, id, tenantID, uuid.New(), testKeycloakClientID)
	require.NoError(t, err)
	return id
}

// seedCredential inserts a service_account_credentials row via the
// superuser pool.
func seedCredential(t testing.TB, ctx context.Context, rawPool *dbseed.Pool, tenantID, principalID uuid.UUID, version int, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO service_account_credentials (id, tenant_id, principal_id, version, status, openbao_path, granted_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		id, tenantID, principalID, version, status,
		fmt.Sprintf("iam/serviceaccount/%s/%s/v%d", tenantID, testKeycloakClientID, version), uuid.New())
	require.NoError(t, err)
	return id
}

// ─────────────────────────────────────────────────────────────────────────
// RLS-1: both tenant-scoped tables carry ENABLE + FORCE RLS + default-deny;
// serviceaccount_app never holds BYPASSRLS; serviceaccount_reconciler holds
// BYPASSRLS but no write grant on either table (RLS-7).
// ─────────────────────────────────────────────────────────────────────────
func TestRLS_EveryTenantScopedTableEnabled(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()

	expected := []string{"service_account_principals", "service_account_credentials"}
	rows, err := rawPool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c JOIN pg_namespace n ON c.relnamespace = n.oid
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		  AND c.relrowsecurity = true AND c.relforcerowsecurity = true`)
	require.NoError(t, err)
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		got[name] = true
	}
	for _, e := range expected {
		assert.True(t, got[e], "expected %s to have ENABLE + FORCE ROW LEVEL SECURITY", e)
	}
	assert.Len(t, got, len(expected), "unexpected extra/missing RLS tables: %v", got)
}

// TestAppRoleLacksBypassRLS — serviceaccount_app is NEVER granted BYPASSRLS
// (§4.4), verified on every CI run.
func TestAppRoleLacksBypassRLS(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()

	var bypass bool
	err := rawPool.QueryRow(ctx, `SELECT rolbypassrls FROM pg_roles WHERE rolname = 'serviceaccount_app'`).Scan(&bypass)
	require.NoError(t, err)
	assert.False(t, bypass, "serviceaccount_app must NOT hold BYPASSRLS")
}

// RLS-7: serviceaccount_reconciler holds BYPASSRLS (to enumerate
// cross-tenant) but has no INSERT/UPDATE/DELETE grant on either
// tenant-scoped table — enumeration-only, never a write path.
func TestReconcilerRoleIsEnumerationOnly(t *testing.T) {
	t.Parallel()
	_, reconcilerPool, rawPool := setupTestDB(t)
	ctx := context.Background()

	var bypass bool
	err := rawPool.QueryRow(ctx, `SELECT rolbypassrls FROM pg_roles WHERE rolname = 'serviceaccount_reconciler'`).Scan(&bypass)
	require.NoError(t, err)
	assert.True(t, bypass, "serviceaccount_reconciler must hold BYPASSRLS to enumerate cross-tenant")

	for _, tbl := range []string{"service_account_principals", "service_account_credentials"} {
		for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
			var has bool
			err := rawPool.QueryRow(ctx,
				`SELECT has_table_privilege('serviceaccount_reconciler', $1, $2)`, tbl, priv).Scan(&has)
			require.NoError(t, err)
			assert.False(t, has, "serviceaccount_reconciler must NOT hold %s on %s (RLS-7)", priv, tbl)
		}
		var hasSelect bool
		err := rawPool.QueryRow(ctx,
			`SELECT has_table_privilege('serviceaccount_reconciler', $1, 'SELECT')`, tbl).Scan(&hasSelect)
		require.NoError(t, err)
		assert.True(t, hasSelect, "serviceaccount_reconciler must hold SELECT on %s", tbl)
	}

	// Functional check: a direct INSERT attempt as serviceaccount_reconciler
	// is rejected at the grant level, not merely absent from a privilege
	// table lookup.
	tenantID := uuid.New()
	err = pgcommon.RunInTx(ctx, reconcilerPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO service_account_principals (tenant_id, principal_sub, keycloak_client_id)
			VALUES ($1, $2, $3)`, tenantID, uuid.New(), testKeycloakClientID)
		return err
	})
	require.Error(t, err, "serviceaccount_reconciler must not be able to write despite BYPASSRLS (RLS-7)")
	assert.Contains(t, err.Error(), "permission denied")
}

// outbox_events / processed_events are RLS-exempt (§4.3).
func TestRLS_OperationalTablesExempt(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()

	for _, tbl := range []string{"outbox_events", "processed_events"} {
		var rls, force bool
		err := rawPool.QueryRow(ctx, `
			SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1`, tbl).Scan(&rls, &force)
		require.NoError(t, err)
		assert.False(t, rls, "%s must be RLS-exempt", tbl)
		assert.False(t, force, "%s must be RLS-exempt", tbl)
	}
}

// Case 1 (§14.5 RLS-T1 / §4.4 canonical case 1): missing GUC -> 0 rows
// (fail-closed).
func TestRLS_Case1_MissingGUCReturnsZeroRows(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	seedPrincipal(t, ctx, rawPool, uuid.New())

	var count int
	err := pgcommon.RunInTx(ctx, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM service_account_principals`).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count, "missing GUC must return 0 rows")
}

// Case 2 (§14.5 RLS-T3 / §4.4 canonical case 2): cross-tenant INSERT
// rejected by WITH CHECK.
func TestRLS_Case2_CrossTenantWriteRejected(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()

	ctxA := withTenant(ctx, tenantA)
	err := pgcommon.RunInTx(ctxA, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO service_account_principals (tenant_id, principal_sub, keycloak_client_id)
			VALUES ($1, $2, $3)`, tenantB, uuid.New(), testKeycloakClientID)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "row-level security", "WITH CHECK must reject a cross-tenant insert")
}

// Case 3 (§4.4 canonical case 3): malformed GUC -> 0 rows, no error (the
// app_tenant_id() EXCEPTION handler catches the bad cast).
func TestRLS_Case3_MalformedGUCFailsClosed(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	seedPrincipal(t, ctx, rawPool, uuid.New())

	var count int
	err := pgcommon.RunInTx(ctx, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL app.tenant_id = 'not-a-uuid'`); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM service_account_principals`).Scan(&count)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, count, "malformed GUC must fail closed to 0 rows, not error")
}

// RLS-T2: cross-tenant read isolation.
func TestRLS_CrossTenantReadIsolation(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	seedPrincipal(t, ctx, rawPool, tenantA)
	seedPrincipal(t, ctx, rawPool, tenantB)

	ctxA := withTenant(ctx, tenantA)
	var crossCount, ownCount int
	err := pgcommon.RunInTx(ctxA, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM service_account_principals WHERE tenant_id = $1`, tenantB).Scan(&crossCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM service_account_principals WHERE tenant_id = $1`, tenantA).Scan(&ownCount)
	})
	require.NoError(t, err)
	assert.Equal(t, 0, crossCount, "must not see the other tenant's row")
	assert.Equal(t, 1, ownCount, "must see its own row")
}

// RLS-T5: the composite (principal_id, tenant_id) FK rejects a credential
// row referencing a principal in another tenant (Layer 3, §10.1).
func TestRLS_CompositeFKRejectsCrossTenantPrincipal(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantA := uuid.New()
	tenantB := uuid.New()
	principalInA := seedPrincipal(t, ctx, rawPool, tenantA)

	_, err := rawPool.Exec(ctx, `
		INSERT INTO service_account_credentials (tenant_id, principal_id, version, status, openbao_path, granted_by)
		VALUES ($1, $2, 1, 'active', 'iam/serviceaccount/x/platform-automation/v1', $3)`,
		tenantB, principalInA, uuid.New())
	require.Error(t, err, "a credential row must not reference a principal in a different tenant")
}

// uq_sac_one_active (TS-INV-3/CONC-2): at most one active credential per
// principal.
func TestRLS_OnlyOneActiveCredentialPerPrincipal(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	seedCredential(t, ctx, rawPool, tenantID, principalID, 1, "active")

	_, err := rawPool.Exec(ctx, `
		INSERT INTO service_account_credentials (tenant_id, principal_id, version, status, openbao_path, granted_by)
		VALUES ($1, $2, 2, 'active', 'iam/serviceaccount/x/platform-automation/v2', $3)`,
		tenantID, principalID, uuid.New())
	require.Error(t, err, "a second active credential for the same principal must be rejected (uq_sac_one_active)")

	// A 'rotating' row may coexist with the 'active' one (overlap window).
	seedCredential(t, ctx, rawPool, tenantID, principalID, 3, "rotating")
}
