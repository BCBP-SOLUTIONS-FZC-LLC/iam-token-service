//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// TestMigration000004_AppRoleCanDeadLetter: platform-events' outbox runner
// moves an exhausted event into outbox_dead_letters as serviceaccount_app.
// Without the grant that move failed and the poison event retried forever.
func TestMigration000004_AppRoleCanDeadLetter(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()
	id := uuid.New()

	err := pgcommon.RunInTx(ctx, appPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_dead_letters (id, event_type, payload, attempts, created_at)
			VALUES ($1, 'ServiceAccountRevoked', '{}'::jsonb, 5, now())`, id); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM outbox_dead_letters WHERE id = $1`, id).Scan(&n); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM outbox_dead_letters WHERE id = $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT nextval('outbox_events_ordering_seq')`)
		return err
	})
	require.NoError(t, err, "serviceaccount_app must be able to dead-letter, list, discard and draw ordering sequence values")
}

// TestMigration000004_RLSPruneEnforcesMinimumRetention: a compromised app
// credential cannot erase RLS-violation evidence younger than 7 days.
func TestMigration000004_RLSPruneEnforcesMinimumRetention(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	repo := pgadapter.NewRLSViolationRepository(appPool)

	_, err := repo.Prune(context.Background(), 1, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least 7")

	_, err = repo.Prune(context.Background(), 7, 100)
	require.NoError(t, err)
}

// TestMigration000004_AppTenantIDHasPinnedSearchPath: SECURITY DEFINER
// functions must not resolve names through the caller's search_path.
func TestMigration000004_AppTenantIDHasPinnedSearchPath(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	var cfg []string
	require.NoError(t, rawPool.QueryRow(context.Background(),
		`SELECT coalesce(proconfig, '{}') FROM pg_proc WHERE proname = 'app_tenant_id'`).Scan(&cfg))
	assert.True(t, strings.Contains(strings.Join(cfg, ","), "search_path=pg_catalog"), "proconfig = %v", cfg)
}
