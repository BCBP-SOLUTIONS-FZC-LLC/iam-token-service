//go:build integration

package postgres_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"
)

// migrateWarnLogger records the "step" field of every Warn call — the
// signal Migrate's tolerated-newer-schema path emits.
type migrateWarnLogger struct {
	mu    sync.Mutex
	steps []string
}

func (*migrateWarnLogger) Debug(string, map[string]interface{}) {}
func (*migrateWarnLogger) Info(string, map[string]interface{})  {}
func (*migrateWarnLogger) Error(string, map[string]interface{}) {}
func (l *migrateWarnLogger) Warn(_ string, f map[string]interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := f["step"].(string); ok {
		l.steps = append(l.steps, s)
	}
}

// TestMigrate_AlreadyAtHeadIsANoOp: Migrate (outbox schema, then domain
// migrations) against a database setupTestDB already brought to head
// succeeds without logging the newer-schema warning, and leaves both
// tracking tables at their head versions.
func TestMigrate_AlreadyAtHeadIsANoOp(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	dsn := rawPool.Config().ConnString()

	domainRunner := &pgmigrate.Runner{FS: pgadapter.MigrationsFS(), DSN: dsn}
	head, _, err := domainRunner.Version(ctx)
	require.NoError(t, err)

	log := &migrateWarnLogger{}
	require.NoError(t, pgadapter.Migrate(ctx, dsn, log))
	assert.Empty(t, log.steps)

	v, dirty, err := domainRunner.Version(ctx)
	require.NoError(t, err)
	assert.False(t, dirty)
	assert.Equal(t, head, v)
}

// TestMigrate_ToleratesDatabaseNewerThanBinary: the image-rollback case —
// both tracking tables record a version this binary has no migration for
// (pgmigrate.ErrVersionNotInSource). Migrate logs and continues for both
// steps instead of failing startup.
func TestMigrate_ToleratesDatabaseNewerThanBinary(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	dsn := rawPool.Config().ConnString()

	_, err := rawPool.Exec(ctx, `UPDATE `+outbox.MigrationsTable+` SET version = 999999`)
	require.NoError(t, err)
	_, err = rawPool.Exec(ctx, `UPDATE `+pgmigrate.DefaultMigrationsTable+` SET version = 999999`)
	require.NoError(t, err)

	log := &migrateWarnLogger{}
	require.NoError(t, pgadapter.Migrate(ctx, dsn, log))
	assert.Equal(t, []string{"outbox schema", "domain migrations"}, log.steps)

	// Nothing was rolled back or re-applied: the newer version still stands.
	var v int64
	require.NoError(t, rawPool.QueryRow(ctx, `SELECT version FROM `+pgmigrate.DefaultMigrationsTable).Scan(&v))
	assert.Equal(t, int64(999999), v)
}

// TestMigrate_DirtyDomainSchemaIsFatal: any domain-migration failure other
// than a newer schema (here, a dirty tracking row) is returned wrapped as
// "domain migrations", after the outbox step succeeded.
func TestMigrate_DirtyDomainSchemaIsFatal(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	dsn := rawPool.Config().ConnString()

	_, err := rawPool.Exec(ctx, `UPDATE `+pgmigrate.DefaultMigrationsTable+` SET dirty = true`)
	require.NoError(t, err)

	log := &migrateWarnLogger{}
	err = pgadapter.Migrate(ctx, dsn, log)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "domain migrations:")
	assert.NotErrorIs(t, err, pgmigrate.ErrVersionNotInSource)
	assert.Empty(t, log.steps)
}

// captureEventPublisher is a port.EventPublisher stand-in; only its identity
// matters here.
type captureEventPublisher struct{}

func (*captureEventPublisher) Enqueue(context.Context, *domain.Event) error { return nil }

// TestTxRunner_RunInTx_InjectsEventPublisher: a TxRunner built with a
// publisher binds both the tx and that publisher into fn's ctx (EVT-1), so
// services can enqueue through port.EventPublisherFromContext.
func TestTxRunner_RunInTx_InjectsEventPublisher(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := withTenant(context.Background(), uuid.New())
	pub := &captureEventPublisher{}

	called := false
	err := pgadapter.NewTxRunner(appPool, pub).RunInTx(ctx, func(txCtx context.Context) error {
		called = true
		_, hasTx := port.TxFromContext(txCtx)
		assert.True(t, hasTx, "fn's ctx carries the transaction")
		got, ok := port.EventPublisherFromContext(txCtx)
		require.True(t, ok, "fn's ctx carries the publisher")
		assert.Same(t, pub, got)
		return nil
	})
	require.NoError(t, err)
	assert.True(t, called)
}
