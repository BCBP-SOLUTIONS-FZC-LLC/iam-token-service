package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
)

// fakeTx is a minimal pgx.Tx — embedding the nil interface panics if any
// method beyond identity comparison is called, which WithTx/TxFromContext
// never do (they only stash/retrieve the value).
type fakeTx struct{ pgx.Tx }

// ── WithTx / TxFromContext ────────────────────────────────────────────

func TestWithTx_TxFromContext_RoundTrip(t *testing.T) {
	tx := &fakeTx{}
	ctx := WithTx(context.Background(), tx)
	got, ok := TxFromContext(ctx)
	require.True(t, ok)
	assert.Same(t, tx, got)
}

func TestTxFromContext_NoTxSetReturnsFalse(t *testing.T) {
	got, ok := TxFromContext(context.Background())
	assert.False(t, ok)
	assert.Nil(t, got)
}

// ── DSNFromEnv / MigrationDSNFromEnv / ReconcilerDSNFromEnv ───────────────

func TestDSNFromEnv_UsesDatabaseURLVerbatim(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://app:pw@db:5432/serviceaccount?sslmode=require")
	// DATABASE_URL takes precedence over PG_* vars in pgcommon.ConfigFromEnv
	// and is returned as-is.
	got := DSNFromEnv()
	assert.Equal(t, "postgres://app:pw@db:5432/serviceaccount?sslmode=require", got)
}

func TestDSNFromEnv_BuildsFromPartsWithoutTimeoutOptions(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PG_HOST", "db")
	t.Setenv("PG_PORT", "5432")
	t.Setenv("PG_USER", "app")
	t.Setenv("PG_PASSWORD", "pw")
	t.Setenv("PG_DBNAME", "serviceaccount")
	t.Setenv("PG_SSLMODE", "disable")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	got := DSNFromEnv()
	require.NotEmpty(t, got)
	assert.NotContains(t, got, "statement_timeout",
		"the statement timeout is applied per transaction by pgcommon (Config.StatementTimeout), never baked into a DSN")
}

func TestMigrationDSNFromEnv(t *testing.T) {
	t.Run("uses MIGRATION_DATABASE_URL when set", func(t *testing.T) {
		t.Setenv("MIGRATION_DATABASE_URL", "postgres://migrator:pw@db:5432/serviceaccount")
		t.Setenv("PG_STATEMENT_TIMEOUT", "")
		assert.Equal(t, "postgres://migrator:pw@db:5432/serviceaccount", MigrationDSNFromEnv())
	})
	t.Run("falls back to DSNFromEnv when unset", func(t *testing.T) {
		t.Setenv("MIGRATION_DATABASE_URL", "")
		t.Setenv("DATABASE_URL", "postgres://app:pw@db:5432/serviceaccount")
		assert.Equal(t, "postgres://app:pw@db:5432/serviceaccount", MigrationDSNFromEnv())
	})
}

func TestReconcilerDSNFromEnv(t *testing.T) {
	t.Run("uses RECONCILER_DATABASE_URL when set", func(t *testing.T) {
		t.Setenv("RECONCILER_DATABASE_URL", "postgres://reconciler:pw@db:5432/serviceaccount")
		t.Setenv("PG_STATEMENT_TIMEOUT", "")
		assert.Equal(t, "postgres://reconciler:pw@db:5432/serviceaccount", ReconcilerDSNFromEnv())
	})
	t.Run("falls back to DSNFromEnv when unset", func(t *testing.T) {
		t.Setenv("RECONCILER_DATABASE_URL", "")
		t.Setenv("DATABASE_URL", "postgres://app:pw@db:5432/serviceaccount")
		assert.Equal(t, "postgres://app:pw@db:5432/serviceaccount", ReconcilerDSNFromEnv())
	})
}

// ── SystemPoolConfig ───────────────────────────────────────────────────

func TestSystemPoolConfig_ForcesPGBouncerMode(t *testing.T) {
	t.Setenv("PG_BOUNCER_MODE", "false")
	t.Setenv("PG_MAX_CONNS", "20")
	t.Setenv("PG_SLOW_QUERY_THRESHOLD", "200ms")
	t.Setenv("PG_STATEMENT_TIMEOUT", "")

	cfg := SystemPoolConfig("postgres://sys@host/db", nil)
	assert.Equal(t, "postgres://sys@host/db", cfg.DSN)
	assert.True(t, cfg.PGBouncerMode, "reconciler pool must force PGBouncerMode:true — zero-value false breaks PgBouncer txn pooling")
	assert.Nil(t, cfg.GUCProvider, "reconciler pool must not inject tenant GUCs")
	assert.Nil(t, cfg.Tracer, "Tracer is wired by the call site, not SystemPoolConfig")
	assert.Nil(t, cfg.Logger, "nil log must leave Logger unset")
	assert.Equal(t, int32(20), cfg.MaxConns, "reconciler pool inherits pool sizing from ConfigFromEnv")
	assert.Equal(t, SystemPoolName, cfg.PoolName,
		"reconciler pool needs its own platform_db_* pool label — same-named live pools share (sum) one gauge collector")
}

func TestSystemPoolConfig_NonNilLoggerWiresAdapter(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "")
	cfg := SystemPoolConfig("postgres://sys@host/db", &fakePortLogger{})
	assert.NotNil(t, cfg.Logger, "non-nil port.Logger must be wrapped via NewLoggerAdapter")
}

func TestSystemPoolConfig_InheritsTransactionTimeouts(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	t.Setenv("PG_LOCK_TIMEOUT", "2s")
	cfg := SystemPoolConfig("postgres://sys@host/db?sslmode=disable", nil)
	assert.Equal(t, "postgres://sys@host/db?sslmode=disable", cfg.DSN, "DSN is used verbatim")
	assert.Equal(t, 5*time.Second, cfg.StatementTimeout)
	assert.Equal(t, 2*time.Second, cfg.LockTimeout)
}

func TestMigrationDSNFromEnv_IgnoresStatementTimeout(t *testing.T) {
	t.Setenv("MIGRATION_DATABASE_URL", "postgres://migrator:pw@db:5432/serviceaccount")
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	assert.Equal(t, "postgres://migrator:pw@db:5432/serviceaccount", MigrationDSNFromEnv(),
		"migrations must not inherit the API's statement timeout")
}

type fakePortLogger struct{}

func (*fakePortLogger) Debug(string, map[string]interface{}) {}
func (*fakePortLogger) Info(string, map[string]interface{})  {}
func (*fakePortLogger) Warn(string, map[string]interface{})  {}
func (*fakePortLogger) Error(string, map[string]interface{}) {}

// ── wrapConnErr ────────────────────────────────────────────────────────

func pgErr(code string) error {
	return &pgconn.PgError{Code: code, Message: "boom"}
}

func TestWrapConnErr(t *testing.T) {
	t.Run("nil passes through", func(t *testing.T) {
		assert.NoError(t, wrapConnErr(nil))
	})
	t.Run("domain error passes through unchanged", func(t *testing.T) {
		de := domain.NewError(domain.ErrPrincipalNotFound, "not found")
		got := wrapConnErr(de)
		assert.Same(t, de, got)
	})
	t.Run("connectivity SQLSTATE is remapped to ErrDBUnavailable", func(t *testing.T) {
		got := wrapConnErr(pgErr("08006"))
		var de *domain.Error
		require.ErrorAs(t, got, &de)
		assert.Equal(t, domain.ErrDBUnavailable, de.Code)
	})
	t.Run("insufficient-resources SQLSTATE is remapped to ErrDBUnavailable", func(t *testing.T) {
		got := wrapConnErr(pgErr("53300"))
		var de *domain.Error
		require.ErrorAs(t, got, &de)
		assert.Equal(t, domain.ErrDBUnavailable, de.Code)
	})
	t.Run("operator-intervention SQLSTATE is remapped to ErrDBUnavailable", func(t *testing.T) {
		got := wrapConnErr(pgErr("57014"))
		var de *domain.Error
		require.ErrorAs(t, got, &de)
		assert.Equal(t, domain.ErrDBUnavailable, de.Code)
	})
	t.Run("system-error SQLSTATE is remapped to ErrDBUnavailable", func(t *testing.T) {
		got := wrapConnErr(pgErr("58000"))
		var de *domain.Error
		require.ErrorAs(t, got, &de)
		assert.Equal(t, domain.ErrDBUnavailable, de.Code)
	})
	t.Run("closed pool is remapped to ErrDBUnavailable", func(t *testing.T) {
		got := wrapConnErr(puddle.ErrClosedPool)
		var de *domain.Error
		require.ErrorAs(t, got, &de)
		assert.Equal(t, domain.ErrDBUnavailable, de.Code)
	})
	t.Run("transport-level error passes through unchanged", func(t *testing.T) {
		src := errors.New("dial tcp: connection refused")
		got := wrapConnErr(src)
		assert.Same(t, src, got)
	})
	t.Run("pgx.ErrNoRows passes through unchanged", func(t *testing.T) {
		got := wrapConnErr(pgx.ErrNoRows)
		assert.ErrorIs(t, got, pgx.ErrNoRows)
	})
	t.Run("context.Canceled passes through unchanged", func(t *testing.T) {
		got := wrapConnErr(context.Canceled)
		assert.ErrorIs(t, got, context.Canceled)
	})
	t.Run("context.DeadlineExceeded passes through unchanged", func(t *testing.T) {
		got := wrapConnErr(context.DeadlineExceeded)
		assert.ErrorIs(t, got, context.DeadlineExceeded)
	})
	t.Run("business SQL error passes through unchanged", func(t *testing.T) {
		src := pgErr("23505") // unique_violation
		got := wrapConnErr(src)
		assert.Same(t, src, got)
	})
	t.Run("unrelated error passes through unchanged", func(t *testing.T) {
		src := errors.New("some business error")
		got := wrapConnErr(src)
		assert.Same(t, src, got)
	})
}

// SQLSTATE 57014 is a DB problem only when the request is still live
// (statement_timeout); when ctx ended (client disconnect, request deadline)
// pgx cancelled the statement itself and it must not count as db_unavailable.
func TestWrapConnErrCtx_QueryCanceled(t *testing.T) {
	canceled := pgErr("57014")

	t.Run("statement timeout on a live request is db_unavailable", func(t *testing.T) {
		err := wrapConnErrCtx(context.Background(), canceled)
		var de *domain.Error
		require.ErrorAs(t, err, &de)
		assert.Equal(t, domain.ErrDBUnavailable, de.Code)
	})
	t.Run("cancellation after ctx ended passes through unchanged", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assert.Same(t, canceled, wrapConnErrCtx(ctx, canceled))
	})
	t.Run("other class 57 errors stay db_unavailable even after ctx ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var de *domain.Error
		require.ErrorAs(t, wrapConnErrCtx(ctx, pgErr("57P01")), &de) // admin_shutdown
		assert.Equal(t, domain.ErrDBUnavailable, de.Code)
	})
}

// SQLSTATE 55P03 (lock_timeout) maps to the retryable 409 rotation_in_flight
// wherever in the adapter it surfaces — never a raw PgError (a 500).
func TestWrapConnErrCtx_LockNotAvailable(t *testing.T) {
	var de *domain.Error
	require.ErrorAs(t, wrapConnErr(fmt.Errorf("wrapped: %w", pgErr("55P03"))), &de)
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
	assert.Equal(t, 409, de.Status())
}
