package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"

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
	// and, per DSNFromEnv's doc comment, is returned as-is (ApplyStatementTimeout
	// is skipped in this branch).
	got := DSNFromEnv()
	assert.Equal(t, "postgres://app:pw@db:5432/serviceaccount?sslmode=require", got)
}

func TestDSNFromEnv_BuildsFromPartsAndAppliesStatementTimeout(t *testing.T) {
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
	assert.Contains(t, got, "statement_timeout%3D5000")
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

// ── ApplyStatementTimeout ──────────────────────────────────────────────

func TestApplyStatementTimeout(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		timeout string
		want    string
	}{
		{"empty dsn is untouched", "", "5s", ""},
		{"unset timeout is a no-op", "postgres://x", "", "postgres://x"},
		{"invalid duration is a no-op", "postgres://x", "not-a-duration", "postgres://x"},
		{"zero duration is a no-op", "postgres://x", "0s", "postgres://x"},
		{"negative duration is a no-op", "postgres://x", "-5s", "postgres://x"},
		{"applies a valid duration", "postgres://x", "500ms", "postgres://x&options=-c%20statement_timeout%3D500"},
		{"idempotent when already present", "postgres://x?options=-c%20statement_timeout%3D999", "5s", "postgres://x?options=-c%20statement_timeout%3D999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PG_STATEMENT_TIMEOUT", tc.timeout)
			assert.Equal(t, tc.want, ApplyStatementTimeout(tc.dsn))
		})
	}
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
}

func TestSystemPoolConfig_NonNilLoggerWiresAdapter(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "")
	cfg := SystemPoolConfig("postgres://sys@host/db", &fakePortLogger{})
	assert.NotNil(t, cfg.Logger, "non-nil port.Logger must be wrapped via NewLoggerAdapter")
}

func TestSystemPoolConfig_AppliesStatementTimeout(t *testing.T) {
	t.Setenv("PG_STATEMENT_TIMEOUT", "5s")
	cfg := SystemPoolConfig("postgres://sys@host/db?sslmode=disable", nil)
	assert.Contains(t, cfg.DSN, "statement_timeout%3D5000")
}

type fakePortLogger struct{}

func (*fakePortLogger) Debug(string, map[string]interface{}) {}
func (*fakePortLogger) Info(string, map[string]interface{})  {}
func (*fakePortLogger) Warn(string, map[string]interface{})  {}
func (*fakePortLogger) Error(string, map[string]interface{}) {}

// ── isNetworkError ──────────────────────────────────────────────────────

// isNetworkError is only ever called from wrapConnErr after an explicit
// `err == nil` guard — it panics on a literal nil (dereferences err.Error()
// unconditionally at the bottom), so nil is deliberately not exercised
// here as it is not part of the function's real call contract.
func TestIsNetworkError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"net.OpError", &net.OpError{Op: "dial", Err: errors.New("boom")}, true},
		{"ECONNRESET", syscall.ECONNRESET, true},
		{"ECONNREFUSED", syscall.ECONNREFUSED, true},
		{"EPIPE", syscall.EPIPE, true},
		{"ETIMEDOUT", syscall.ETIMEDOUT, true},
		{"connection refused message", errors.New("dial tcp: connection refused"), true},
		{"connection reset message", errors.New("read: connection reset by peer"), true},
		{"broken pipe message", errors.New("write: broken pipe"), true},
		{"EOF in message", errors.New("unexpected EOF"), true},
		{"unrelated syscall errno", syscall.EACCES, false},
		{"unrelated error", errors.New("unique constraint violation"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isNetworkError(tc.err))
		})
	}
}

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
	t.Run("network error is remapped to ErrDBUnavailable", func(t *testing.T) {
		got := wrapConnErr(io.EOF)
		var de *domain.Error
		require.ErrorAs(t, got, &de)
		assert.Equal(t, domain.ErrDBUnavailable, de.Code)
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
