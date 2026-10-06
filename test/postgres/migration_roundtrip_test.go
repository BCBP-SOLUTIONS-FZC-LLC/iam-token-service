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

// TestMigrations_DownUpRoundTrip: every down migration actually reverses its
// up — all the way down to an empty domain schema and back up again without
// error — so a rollback is a tested path rather than an untried script.
// Written against "however many migrations exist" (Version + Down(n)) so a
// newly-added migration is covered without editing this test. Also checks
// one concrete pre-000004 state on the way down: 000004 raised
// prune_rls_violation_log's minimum ttl to 7 days, and its down restores
// 000003's minimum of 1.
func TestMigrations_DownUpRoundTrip(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	runner := &pgmigrate.Runner{FS: pgadapter.MigrationsFS(), DSN: rawPool.Config().ConnString()}

	head, dirty, err := runner.Version(ctx)
	require.NoError(t, err)
	require.False(t, dirty)
	require.GreaterOrEqual(t, head, uint(4), "setupTestDB applies at least 000001..000004")

	// Down to 000003: 000004's prune floor (7 days) is gone again.
	require.NoError(t, runner.Down(ctx, int(head-3)))
	v, dirty, err := runner.Version(ctx)
	require.NoError(t, err)
	require.False(t, dirty)
	require.Equal(t, uint(3), v)
	var pruned int
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT prune_rls_violation_log(1, 10)`).Scan(&pruned),
		"pre-000004 prune accepts ttl_days below 7")

	// The rest of the way down: no domain table survives.
	require.NoError(t, runner.Down(ctx, 3))
	var tables int
	require.NoError(t, rawPool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name IN
			('service_account_principals', 'service_account_credentials', 'rls_violation_log')`).Scan(&tables))
	assert.Zero(t, tables)

	// And back up to head.
	require.NoError(t, runner.Up(ctx))
	v, dirty, err = runner.Version(ctx)
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.Equal(t, head, v)

	err = rawPool.QueryRow(ctx, `SELECT prune_rls_violation_log(1, 10)`).Scan(&pruned)
	require.Error(t, err, "re-applied 000004 restores the 7-day floor")
	assert.Contains(t, err.Error(), "at least 7")
}
