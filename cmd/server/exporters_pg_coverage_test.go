//go:build integration

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// migratedPool starts a throwaway Postgres, applies the outbox schema and
// this service's migrations (the same order as test/postgres's setupTestDB),
// and returns a superuser pgcommon.Pool — a stand-in for the BYPASSRLS
// reconciler pool the rotation-overlap exporter reads through.
func migratedPool(t *testing.T) *pgcommon.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping postgres-backed exporter test in short mode")
	}
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("serviceaccount"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("testpassword"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgcommon.NewPool(ctx, pgcommon.Config{DSN: dsn})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// The migrations GRANT to these roles; they only need to exist.
	for _, role := range []string{"serviceaccount_app", "serviceaccount_migrator", "serviceaccount_reconciler", "admin_readonly"} {
		require.NoError(t, execSQL(ctx, pool, fmt.Sprintf(`CREATE ROLE %s NOLOGIN`, role)))
	}
	require.NoError(t, outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn}))
	require.NoError(t, pgadapter.RunMigrations(ctx, dsn))
	return pool
}

func execSQL(ctx context.Context, pool *pgcommon.Pool, sql string, args ...any) error {
	return pool.WithTx(ctx, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// seedCredentials inserts one principal with `active`, `rotating` (with an
// overlap expiry) and soft-deleted `rotating` credentials — only the live
// rotating row with expires_at set counts towards the overlap gauge.
func seedCredentials(t *testing.T, pool *pgcommon.Pool) {
	t.Helper()
	ctx := context.Background()
	tenantID, principalID := uuid.New(), uuid.New()
	require.NoError(t, execSQL(ctx, pool, `
		INSERT INTO service_account_principals (id, tenant_id, principal_sub, keycloak_client_id)
		VALUES ($1, $2, $3, 'platform-automation')`, principalID, tenantID, uuid.New()))
	insert := func(version int, status string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, execSQL(ctx, pool, `
			INSERT INTO service_account_credentials (id, tenant_id, principal_id, version, status, openbao_path, granted_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, tenantID, principalID, version, status,
			fmt.Sprintf("iam/serviceaccount/%s/platform-automation/v%d", tenantID, version), uuid.New()))
		return id
	}
	insert(3, "active")
	live := insert(2, "rotating")
	require.NoError(t, execSQL(ctx, pool,
		`UPDATE service_account_credentials SET expires_at = now() + interval '1 hour' WHERE id = $1`, live))
}

func TestRefreshRotationOverlapGauge_CountsLiveRotatingAndKeepsLastOnError(t *testing.T) {
	metrics.Register("test")
	pool := migratedPool(t)
	seedCredentials(t, pool)

	repo := pgadapter.NewReconcilerRepository(pool)
	metrics.RotationOverlapActive.Set(-1)
	refreshRotationOverlapGauge(t.Context(), repo, nil)
	assert.InDelta(t, 1, testutil.ToFloat64(metrics.RotationOverlapActive), 0,
		"exactly one rotating row with an overlap expiry")

	// A query failure (here: a closed pool) leaves the gauge at its last
	// value and is logged, with the trace id when one is in context.
	pool.Close()
	log := &warnLogger{}
	ctx, traceID := tracedCtx(t)
	refreshRotationOverlapGauge(ctx, repo, log)
	refreshRotationOverlapGauge(t.Context(), repo, nil) // nil logger: silent
	assert.InDelta(t, 1, testutil.ToFloat64(metrics.RotationOverlapActive), 0)
	warns, fields := log.snapshot()
	require.Equal(t, []string{"rotation overlap exporter: query failed"}, warns)
	assert.NotEmpty(t, fields[0]["error"])
	assert.Equal(t, traceID, fields[0]["trace_id"])

	refreshRotationOverlapGauge(t.Context(), repo, log)
	_, fields = log.snapshot()
	require.Len(t, fields, 2)
	assert.NotContains(t, fields[1], "trace_id")
}

func TestRunRotationOverlapExporter_RefreshesUntilCancelled(t *testing.T) {
	metrics.Register("test")
	pool := migratedPool(t)

	repo := pgadapter.NewReconcilerRepository(pool)
	metrics.RotationOverlapActive.Set(-1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runRotationOverlapExporter(ctx, repo, 5*time.Millisecond, nil)
		close(done)
	}()
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.RotationOverlapActive) == 0 },
		10*time.Second, 5*time.Millisecond, "the initial refresh sets the gauge from the empty table")

	// A row seeded after start is picked up by a ticker-driven refresh.
	seedCredentials(t, pool)
	require.Eventually(t, func() bool { return testutil.ToFloat64(metrics.RotationOverlapActive) == 1 },
		10*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("exporter did not stop on context cancellation")
	}
}
