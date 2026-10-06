//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// keys_refresh_pending (migration 000005) is the durable "RP-17 key-cache
// refresh owed for this tenant" marker cmd/rotator and cmd/scheduler write
// before/with each revoke/rotation and clear only after RP-17 succeeds.

func markerPending(t *testing.T, rawPool *dbseed.Pool, tenantID uuid.UUID) bool {
	t.Helper()
	var n int
	require.NoError(t, rawPool.QueryRow(context.Background(),
		`SELECT count(*) FROM keys_refresh_pending WHERE tenant_id = $1`, tenantID).Scan(&n))
	return n == 1
}

// Migration 000005: RLS-exempt operational table; the app role may read and
// write it, the BYPASSRLS reconciler role may only read it (RLS-7).
func TestMigration000005_KeysRefreshPendingShapeAndGrants(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()

	var rls, force bool
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'keys_refresh_pending'`).Scan(&rls, &force))
	assert.False(t, rls, "keys_refresh_pending is RLS-exempt")
	assert.False(t, force)

	for priv, want := range map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true} {
		var has bool
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT has_table_privilege('serviceaccount_app', 'keys_refresh_pending', $1)`, priv).Scan(&has))
		assert.Equal(t, want, has, "serviceaccount_app %s", priv)
	}
	for priv, want := range map[string]bool{"SELECT": true, "INSERT": false, "UPDATE": false, "DELETE": false} {
		var has bool
		require.NoError(t, rawPool.QueryRow(ctx, `SELECT has_table_privilege('serviceaccount_reconciler', 'keys_refresh_pending', $1)`, priv).Scan(&has))
		assert.Equal(t, want, has, "serviceaccount_reconciler %s", priv)
	}
}

// The marker is written in the same transaction as the revoke it covers: a
// rolled-back revoke leaves no marker, a committed one always does.
func TestKeysRefresh_MarkerIsTransactional(t *testing.T) {
	t.Parallel()
	appPool, reconcilerPool, rawPool := setupTestDB(t)
	repo := pgadapter.NewKeysRefreshRepository(appPool, reconcilerPool)
	tx := pgadapter.NewTxRunner(appPool, nil)
	tenantID := uuid.New()
	ctx := withTenant(context.Background(), tenantID)

	boom := errors.New("revoke failed after the marker write")
	err := tx.RunInTx(ctx, func(ctx context.Context) error {
		if _, err := repo.MarkPending(ctx, tenantID); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)
	assert.False(t, markerPending(t, rawPool, tenantID), "a rolled-back revoke leaves no marker")

	var mark pgadapter.KeysRefreshMark
	require.NoError(t, tx.RunInTx(ctx, func(ctx context.Context) error {
		var err error
		mark, err = repo.MarkPending(ctx, tenantID)
		return err
	}))
	assert.True(t, markerPending(t, rawPool, tenantID))
	assert.True(t, mark.Fresh, "no refresh was owed before")
}

// A second mark updates in place (Fresh=false) and only ever moves
// requested_at forward.
func TestKeysRefresh_MarkPendingUpsertsMonotonically(t *testing.T) {
	t.Parallel()
	appPool, reconcilerPool, _ := setupTestDB(t)
	repo := pgadapter.NewKeysRefreshRepository(appPool, reconcilerPool)
	tenantID := uuid.New()
	ctx := withTenant(context.Background(), tenantID)

	first, err := repo.MarkPending(ctx, tenantID)
	require.NoError(t, err)
	second, err := repo.MarkPending(ctx, tenantID)
	require.NoError(t, err)

	assert.True(t, first.Fresh)
	assert.False(t, second.Fresh)
	assert.True(t, second.RequestedAt.After(first.RequestedAt))
}

// Full lifecycle: set with a revoke, survives a failed RP-17 (never
// cleared), listed for the next run's retry, then cleared once RP-17
// succeeds. A request written after the value the RP-17 call observed
// survives that clear.
func TestKeysRefresh_MarkerLifecycle(t *testing.T) {
	t.Parallel()
	appPool, reconcilerPool, rawPool := setupTestDB(t)
	repo := pgadapter.NewKeysRefreshRepository(appPool, reconcilerPool)
	tenantID := uuid.New()
	seedPrincipal(t, context.Background(), rawPool, tenantID)
	ctx := withTenant(context.Background(), tenantID)

	mark, err := repo.MarkPending(ctx, tenantID)
	require.NoError(t, err)

	// RP-17 failed: the caller never clears. The next run lists it at once:
	// a committed marker has no age guard.
	pending, err := repo.ListPending(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, tenantID, pending[0].TenantID)
	assert.True(t, pending[0].HasPrincipal)
	assert.WithinDuration(t, mark.RequestedAt, pending[0].RequestedAt, time.Microsecond)

	// A concurrent run requests another refresh while this retry's RP-17 is
	// in flight: clearing up to the observed value must leave it.
	newer, err := repo.MarkPending(ctx, tenantID)
	require.NoError(t, err)
	require.NoError(t, repo.Clear(ctx, tenantID, pending[0].RequestedAt))
	assert.True(t, markerPending(t, rawPool, tenantID), "a newer request survives an older refresh's clear")

	// Its own refresh then clears it.
	require.NoError(t, repo.Clear(ctx, tenantID, newer.RequestedAt))
	assert.False(t, markerPending(t, rawPool, tenantID))
	pending, err = repo.ListPending(context.Background(), 100)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// A live intent marker (cmd/scheduler, before its rotation commits) is left
// to its writer; an expired one is retried. HasPrincipal is false once the
// tenant is offboarded.
func TestKeysRefresh_ListPendingSkipsLiveIntentsAndReportsOffboarded(t *testing.T) {
	t.Parallel()
	appPool, reconcilerPool, rawPool := setupTestDB(t)
	repo := pgadapter.NewKeysRefreshRepository(appPool, reconcilerPool)
	ctx := context.Background()

	liveIntent := uuid.New()
	_, err := repo.MarkIntent(withTenant(ctx, liveIntent), liveIntent, 10*time.Minute)
	require.NoError(t, err)

	expiredIntent := uuid.New()
	_, err = rawPool.Exec(ctx, `INSERT INTO keys_refresh_pending (tenant_id, requested_at, intent_until) VALUES ($1, now() - interval '10 minutes', now() - interval '1 minute')`, expiredIntent)
	require.NoError(t, err)

	offboarded := uuid.New()
	_, err = rawPool.Exec(ctx, `INSERT INTO keys_refresh_pending (tenant_id, requested_at) VALUES ($1, now() - interval '1 hour')`, offboarded)
	require.NoError(t, err)

	pending, err := repo.ListPending(ctx, 100)
	require.NoError(t, err)
	require.Len(t, pending, 2, "the live intent is left to its writer")
	assert.Equal(t, offboarded, pending[0].TenantID, "oldest first")
	assert.False(t, pending[0].HasPrincipal)
	assert.Equal(t, expiredIntent, pending[1].TenantID, "an expired intent's writer finished or died — retry it")
}

// A just-written committed marker (the sweep's) is listed at once — the old
// global minAge used to hold it back for 4 minutes.
func TestKeysRefresh_CommittedMarkerIsListedImmediately(t *testing.T) {
	t.Parallel()
	appPool, reconcilerPool, _ := setupTestDB(t)
	repo := pgadapter.NewKeysRefreshRepository(appPool, reconcilerPool)
	tenantID := uuid.New()

	_, err := repo.MarkPending(withTenant(context.Background(), tenantID), tenantID)
	require.NoError(t, err)

	pending, err := repo.ListPending(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, tenantID, pending[0].TenantID)
}

func intentUntil(t *testing.T, rawPool *dbseed.Pool, tenantID uuid.UUID) *time.Time {
	t.Helper()
	var until *time.Time
	require.NoError(t, rawPool.QueryRow(context.Background(),
		`SELECT intent_until FROM keys_refresh_pending WHERE tenant_id = $1`, tenantID).Scan(&until))
	return until
}

// The intent/committed upsert rules: a committed mark clears an intent; an
// intent mark never downgrades a committed marker, nor revives an expired
// intent; it extends a live one; requested_at stays monotonic throughout.
func TestKeysRefresh_IntentAndCommittedUpsertRules(t *testing.T) {
	t.Parallel()
	appPool, reconcilerPool, rawPool := setupTestDB(t)
	repo := pgadapter.NewKeysRefreshRepository(appPool, reconcilerPool)
	ctx := context.Background()

	t.Run("committed mark converts an intent", func(t *testing.T) {
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		intent, err := repo.MarkIntent(tctx, tenantID, 4*time.Minute)
		require.NoError(t, err)
		assert.True(t, intent.Fresh)
		until := intentUntil(t, rawPool, tenantID)
		require.NotNil(t, until)
		assert.WithinDuration(t, intent.RequestedAt.Add(4*time.Minute), *until, time.Second)

		committed, err := repo.MarkPending(tctx, tenantID)
		require.NoError(t, err)
		assert.False(t, committed.Fresh)
		assert.True(t, committed.RequestedAt.After(intent.RequestedAt))
		assert.Nil(t, intentUntil(t, rawPool, tenantID))
	})

	t.Run("intent mark never downgrades a committed marker", func(t *testing.T) {
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		committed, err := repo.MarkPending(tctx, tenantID)
		require.NoError(t, err)

		intent, err := repo.MarkIntent(tctx, tenantID, 4*time.Minute)
		require.NoError(t, err)
		assert.False(t, intent.Fresh)
		assert.True(t, intent.RequestedAt.After(committed.RequestedAt))
		assert.Nil(t, intentUntil(t, rawPool, tenantID), "still committed — retryable at once")
	})

	t.Run("intent mark does not revive an expired intent", func(t *testing.T) {
		tenantID := uuid.New()
		_, err := rawPool.Exec(ctx, `INSERT INTO keys_refresh_pending (tenant_id, requested_at, intent_until) VALUES ($1, now() - interval '10 minutes', now() - interval '1 minute')`, tenantID)
		require.NoError(t, err)

		_, err = repo.MarkIntent(withTenant(ctx, tenantID), tenantID, 4*time.Minute)
		require.NoError(t, err)
		assert.Nil(t, intentUntil(t, rawPool, tenantID), "its writer died: treat as committed, never delay it again")
	})

	t.Run("intent mark extends a live intent", func(t *testing.T) {
		tenantID := uuid.New()
		tctx := withTenant(ctx, tenantID)
		_, err := repo.MarkIntent(tctx, tenantID, time.Minute)
		require.NoError(t, err)
		first := intentUntil(t, rawPool, tenantID)

		_, err = repo.MarkIntent(tctx, tenantID, 5*time.Minute)
		require.NoError(t, err)
		second := intentUntil(t, rawPool, tenantID)
		require.NotNil(t, first)
		require.NotNil(t, second)
		assert.True(t, second.After(first.Add(3*time.Minute)))
	})
}

// PendingStats counts every marker (committed and intent alike) and ages the
// oldest; an empty table reports zeroes.
func TestKeysRefresh_PendingStats(t *testing.T) {
	t.Parallel()
	appPool, reconcilerPool, rawPool := setupTestDB(t)
	repo := pgadapter.NewKeysRefreshRepository(appPool, reconcilerPool)
	ctx := context.Background()

	_, err := rawPool.Exec(ctx, `DELETE FROM keys_refresh_pending`)
	require.NoError(t, err)
	count, age, err := repo.PendingStats(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.Zero(t, age)

	_, err = rawPool.Exec(ctx, `INSERT INTO keys_refresh_pending (tenant_id, requested_at) VALUES ($1, now() - interval '90 seconds')`, uuid.New())
	require.NoError(t, err)
	intentTenant := uuid.New()
	_, err = repo.MarkIntent(withTenant(ctx, intentTenant), intentTenant, 4*time.Minute)
	require.NoError(t, err)

	count, age, err = repo.PendingStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.GreaterOrEqual(t, age, 90*time.Second)
	assert.Less(t, age, 2*time.Minute)
}

// The retry pass enumerates over the reconciler pool, which can read the
// marker table but never write it (RLS-7).
func TestKeysRefresh_ReconcilerRoleCannotWriteMarkers(t *testing.T) {
	t.Parallel()
	_, reconcilerPool, _ := setupTestDB(t)

	err := pgcommon.RunInTx(context.Background(), reconcilerPool, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO keys_refresh_pending (tenant_id) VALUES ($1)`, uuid.New())
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}
