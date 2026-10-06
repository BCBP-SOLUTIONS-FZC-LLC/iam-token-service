//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// unsampledAppPool returns a fresh serviceaccount_app pool whose sessions
// log every RLS violation (app.rls_violation_sample_rate = 1 on the role), so
// assertions on rls_violation_log are deterministic. A fresh pool is needed
// because a role-level setting only applies to new connections.
func unsampledAppPool(t *testing.T, rawPool *dbseed.Pool) *pgcommon.Pool {
	t.Helper()
	ctx := context.Background()
	_, err := rawPool.Exec(ctx, `ALTER ROLE serviceaccount_app SET app.rls_violation_sample_rate = '1'`)
	require.NoError(t, err)
	dsn := strings.Replace(rawPool.Config().ConnString(), "postgres:testpassword@", "serviceaccount_app:"+appRolePassword+"@", 1)
	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn, GUCProvider: pgcommon.GUCSetFromContext})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func violationCounts(t *testing.T, rawPool *dbseed.Pool) map[string]int {
	t.Helper()
	rows, err := rawPool.Query(context.Background(), `SELECT violation_type, count(*) FROM rls_violation_log GROUP BY violation_type`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var vType string
		var n int
		require.NoError(t, rows.Scan(&vType, &n))
		out[vType] = n
	}
	require.NoError(t, rows.Err())
	return out
}

// TestRLSViolationLog_RecordsBothViolationTypes: a query without a tenant
// GUC logs missing_or_invalid_guc, and reading another tenant's row logs
// cross_tenant_access — from inside the STABLE rls_check_tenant() policy
// predicate, which must still fail closed (0 rows).
func TestRLSViolationLog_RecordsBothViolationTypes(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	appPool := unsampledAppPool(t, rawPool)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	principalB := seedPrincipal(t, ctx, rawPool, tenantB)

	var count int
	require.NoError(t, pgcommon.RunInTx(ctx, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM service_account_principals`).Scan(&count)
	}))
	assert.Equal(t, 0, count, "missing GUC must still return 0 rows")

	require.NoError(t, pgcommon.RunInTx(withTenant(ctx, tenantA), appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM service_account_principals WHERE id = $1`, principalB).Scan(&count)
	}))
	assert.Equal(t, 0, count, "another tenant's row must stay hidden")

	counts := violationCounts(t, rawPool)
	assert.Positive(t, counts["missing_or_invalid_guc"], "missing GUC read must be logged")
	assert.Positive(t, counts["cross_tenant_access"], "cross-tenant read must be logged")

	var appTenant, rowTenant uuid.UUID
	var tableName, query string
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT app_tenant_id, row_tenant_id, table_name, query_text FROM rls_violation_log
		 WHERE violation_type = 'cross_tenant_access' LIMIT 1`).Scan(&appTenant, &rowTenant, &tableName, &query))
	assert.Equal(t, tenantA, appTenant)
	assert.Equal(t, tenantB, rowTenant)
	assert.Equal(t, "service_account_principals", tableName)
	assert.Contains(t, query, "WHERE id = $1")
}

// TestRLSViolationLog_RejectedWriteIsNotLogged pins a known limit: a
// cross-tenant INSERT/UPDATE rejected by WITH CHECK raises an error, which
// rolls the log row back with the caller's transaction (Postgres has no
// autonomous transactions). The write is still rejected, and the caller sees
// the error; only cross-tenant reads and missing-GUC queries reach the log.
func TestRLSViolationLog_RejectedWriteIsNotLogged(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	appPool := unsampledAppPool(t, rawPool)
	ctx := context.Background()

	err := pgcommon.RunInTx(withTenant(ctx, uuid.New()), appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO service_account_principals (tenant_id, principal_sub, keycloak_client_id)
			VALUES ($1, $2, $3)`, uuid.New(), uuid.New(), testKeycloakClientID)
		return err
	})
	require.Error(t, err, "WITH CHECK must reject the cross-tenant insert")
	assert.Empty(t, violationCounts(t, rawPool))
}

// TestRLSViolationLog_NoFalsePositivesOnRepositoryPaths runs every tenant-
// scoped repository method under a correct tenant GUC, with other tenants'
// rows present in both tables, and asserts nothing is logged. RLS predicates
// are evaluated on every row a scan visits, so a query that does not filter
// on tenant_id first would log the other tenants' rows as cross_tenant_access
// and turn the alert into noise.
func TestRLSViolationLog_NoFalsePositivesOnRepositoryPaths(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	appPool := unsampledAppPool(t, rawPool)
	ctx := context.Background()

	for range 3 {
		other := uuid.New()
		p := seedPrincipal(t, ctx, rawPool, other)
		seedCredential(t, ctx, rawPool, other, p, 1, "active")
	}

	tenantID := uuid.New()
	tctx := withTenant(ctx, tenantID)
	principals := pgadapter.NewPrincipalRepository(appPool)
	credentials := pgadapter.NewCredentialRepository(appPool)
	tx := pgadapter.NewTxRunner(appPool, nil)

	var principal *domain.ServiceAccountPrincipal
	require.NoError(t, tx.RunInTx(tctx, func(ctx context.Context) error {
		var err error
		principal, _, _, err = principals.Register(ctx, &domain.ServiceAccountPrincipal{
			TenantID: tenantID, PrincipalSub: uuid.New(), KeycloakClientID: testKeycloakClientID,
			PrincipalType: domain.PrincipalTypePlatformAutomation,
		})
		return err
	}))
	rotationID := uuid.New()
	v1 := &domain.Credential{
		TenantID: tenantID, PrincipalID: principal.ID, Version: 1, Status: domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 1), GrantedBy: domain.SystemPrincipalID,
		RotationID: &rotationID,
	}
	require.NoError(t, tx.RunInTx(tctx, func(ctx context.Context) error { return credentials.Insert(ctx, v1) }))

	require.NoError(t, tx.RunInTx(tctx, func(ctx context.Context) error {
		if _, err := principals.FindByID(ctx, tenantID, principal.ID); err != nil {
			return err
		}
		if _, err := principals.FindByPrincipalSub(ctx, tenantID, principal.PrincipalSub); err != nil {
			return err
		}
		if _, err := principals.FindByType(ctx, tenantID, domain.PrincipalTypePlatformAutomation); err != nil {
			return err
		}
		if _, err := principals.ListByTenant(ctx, tenantID); err != nil {
			return err
		}
		if _, err := credentials.FindActive(ctx, tenantID, principal.ID); err != nil {
			return err
		}
		if _, err := credentials.FindByVersion(ctx, tenantID, principal.ID, 1); err != nil {
			return err
		}
		if _, err := credentials.FindByRotationID(ctx, tenantID, principal.ID, rotationID); err != nil {
			return err
		}
		if _, err := credentials.ListByPrincipal(ctx, tenantID, principal.ID); err != nil {
			return err
		}
		v1.Status = domain.CredentialStatusRotating
		if err := credentials.Update(ctx, v1); err != nil {
			return err
		}
		return credentials.Insert(ctx, &domain.Credential{
			TenantID: tenantID, PrincipalID: principal.ID, Version: 2, Status: domain.CredentialStatusActive,
			OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 2), GrantedBy: domain.SystemPrincipalID,
		})
	}))
	// The offboarding cascade's delete.
	require.NoError(t, tx.RunInTx(tctx, func(ctx context.Context) error { return principals.DeleteByTenant(ctx, tenantID) }))

	assert.Empty(t, violationCounts(t, rawPool), "a correctly scoped repository call must never log an RLS violation")
}

// TestRLSViolationLog_ReadAndPruneGrants: the reconciler role (the
// exporter's pool) can read the log, the app role cannot read it directly but
// can prune past-retention rows through prune_rls_violation_log().
func TestRLSViolationLog_ReadAndPruneGrants(t *testing.T) {
	t.Parallel()
	appPool, reconcilerPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO rls_violation_log (table_name, violation_type, occurred_at) VALUES
		  ('service_account_principals', 'cross_tenant_access', now() - interval '40 days'),
		  ('service_account_principals', 'missing_or_invalid_guc', now() - interval '1 minute')`)
	require.NoError(t, err)

	repo := pgadapter.NewRLSViolationRepository(reconcilerPool)
	counts, lastID, err := repo.CountSince(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"cross_tenant_access": 1, "missing_or_invalid_guc": 1}, counts)
	assert.Positive(t, lastID)
	counts, again, err := repo.CountSince(ctx, lastID)
	require.NoError(t, err)
	assert.Empty(t, counts, "rows at or below the cursor are not counted twice")
	assert.Equal(t, lastID, again)

	startID, err := repo.CursorBefore(ctx, time.Hour)
	require.NoError(t, err)
	counts, _, err = repo.CountSince(ctx, startID)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"missing_or_invalid_guc": 1}, counts, "the startup cursor skips rows older than the window")

	err = pgcommon.RunInTx(ctx, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM rls_violation_log`)
		return err
	})
	require.Error(t, err, "the app role must not read the audit table directly")

	deleted, err := pgadapter.NewRLSViolationRepository(appPool).Prune(ctx, 30, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, deleted, "only the row past the 30-day retention is pruned")
}
