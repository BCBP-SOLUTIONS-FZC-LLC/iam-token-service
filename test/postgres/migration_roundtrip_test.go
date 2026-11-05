//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
)

// TestMigrations_DownUpRoundTrip verifies that the down migration fully
// reverses the up migration (no domain table survives) and that re-applying
// up restores the complete hardened schema. The schema is a single
// consolidated migration (000001), so there are no intermediate states to
// visit.
func TestMigrations_DownUpRoundTrip(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	runner := &pgmigrate.Runner{FS: pgadapter.MigrationsFS(), DSN: rawPool.Config().ConnString()}

	head, dirty, err := runner.Version(ctx)
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, uint(1), head, "consolidated schema has exactly one migration (000001)")

	// Down to 0: no domain table survives.
	require.NoError(t, runner.Down(ctx, 1))
	var tables int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN
			('service_account_principals', 'service_account_credentials',
			 'rls_violation_log', 'processed_events', 'keys_refresh_pending')`).Scan(&tables))
	assert.Zero(t, tables, "down migration drops all service tables")

	// Back up to head.
	require.NoError(t, runner.Up(ctx))
	v, dirty, err := runner.Version(ctx)
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.Equal(t, head, v)

	// Consolidated schema includes the 7-day prune minimum from the start.
	var pruned int
	err = rawPool.QueryRow(ctx, `SELECT prune_rls_violation_log(1, 10)`).Scan(&pruned)
	require.Error(t, err, "consolidated schema enforces 7-day prune minimum")
	assert.Contains(t, err.Error(), "at least 7")
}
