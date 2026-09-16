package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/puddle/v2"
)

// DSNFromEnv builds a PostgreSQL connection URL for the application pool by
// delegating host/port/user/password/dbname/sslmode parsing and DSN
// assembly to pgcommon.ConfigFromEnv() — the same env vars
// (DATABASE_URL/PG_HOST/PG_PORT/PG_USER/PG_PASSWORD/PG_DBNAME/PG_SSLMODE)
// platform-pgcommon itself reads to build the pool Config used by
// cmd/server/main.go, so there is exactly one DSN-assembly implementation
// instead of two drifting in parallel (§12).
func DSNFromEnv() string {
	cfg, _ := pgcommon.ConfigFromEnv()
	if os.Getenv("DATABASE_URL") != "" {
		return cfg.DSN
	}
	return ApplyStatementTimeout(cfg.DSN)
}

// ApplyStatementTimeout appends a server-side statement_timeout option to
// dsn so hung queries release pool connections instead of holding them
// indefinitely. PG_STATEMENT_TIMEOUT accepts a Go duration string (e.g.
// "5s", "500ms"). Ignored when dsn is empty or PG_STATEMENT_TIMEOUT is
// unset. Idempotent: a DSN that already carries statement_timeout is
// returned unchanged.
func ApplyStatementTimeout(dsn string) string {
	if dsn == "" {
		return dsn
	}
	if t := os.Getenv("PG_STATEMENT_TIMEOUT"); t != "" {
		if d, err := time.ParseDuration(t); err == nil && d > 0 {
			if strings.Contains(dsn, "statement_timeout") {
				return dsn
			}
			dsn += fmt.Sprintf("&options=-c%%20statement_timeout%%3D%d", d.Milliseconds())
		}
	}
	return dsn
}

// MigrationDSNFromEnv returns the DSN for schema migrations. Migrations
// MUST bypass PgBouncer because the migration runner uses
// pg_advisory_lock, which is session-scoped and breaks under transaction
// pooling (TS-CONFIG-2). MIGRATION_DATABASE_URL must be set whenever
// PG_BOUNCER_MODE=true.
func MigrationDSNFromEnv() string {
	if dsn := os.Getenv("MIGRATION_DATABASE_URL"); dsn != "" {
		return ApplyStatementTimeout(dsn)
	}
	return DSNFromEnv()
}

// ReconcilerDSNFromEnv returns the DSN for the privileged cross-tenant
// serviceaccount_reconciler pool used by cmd/rotator to enumerate candidate
// rows across tenants (§4.3/RLS-7). In production it MUST point to
// serviceaccount_reconciler (NOLOGIN BYPASSRLS, SELECT-only — never a role
// with write privilege). Falls back to DSNFromEnv() for local dev;
// cross-tenant enumeration then returns zero rows under RLS (safe no-op)
// rather than failing to connect.
func ReconcilerDSNFromEnv() string {
	if dsn := os.Getenv("RECONCILER_DATABASE_URL"); dsn != "" {
		return ApplyStatementTimeout(dsn)
	}
	return DSNFromEnv()
}

// SystemPoolConfig returns pgcommon.Config for the BYPASSRLS reconciler
// pool (§4.3/RLS-7), matching iam-user-profile / iam-org-membership.
// The pool deliberately has no GUCProvider — cross-tenant rotator /
// overlap-exporter queries run under serviceaccount_reconciler — but
// still connects through PgBouncer in production, so PGBouncerMode is
// forced true (SimpleProtocol + MinConns:0). A bare
// pgcommon.Config{DSN, Logger} literal would leave PGBouncerMode at the
// Go zero-value false and drop ConfigFromEnv pool sizing.
//
// Pool sizing, lifetimes, and SlowQueryThreshold are copied from
// ConfigFromEnv so the reconciler pool and the app pool share one
// env-driven source of truth. Tracer is left unset — call sites wire
// NewOTelTracer so db.query spans export through gincommon's
// TracerProvider.
func SystemPoolConfig(dsn string, log port.Logger) pgcommon.Config {
	cfg, _ := pgcommon.ConfigFromEnv()
	cfg.DSN = ApplyStatementTimeout(dsn)
	cfg.GUCProvider = nil
	cfg.PGBouncerMode = true
	cfg.Tracer = nil
	if log != nil {
		cfg.Logger = NewLoggerAdapter(log)
	} else {
		cfg.Logger = nil
	}
	return cfg
}

// TxRunner wraps pgcommon.Pool to implement port.TxRunner. When a
// port.EventPublisher is provided it is injected into the tx context so
// services can enqueue events atomically via
// port.EventPublisherFromContext(txCtx) without importing this package
// (EVT-1).
type TxRunner struct {
	pool   *pgcommon.Pool
	events port.EventPublisher
}

// NewTxRunner constructs a TxRunner. Pass nil for events during bootstrap
// paths (e.g. migration-only runs) where no outbox writes occur.
func NewTxRunner(pool *pgcommon.Pool, events port.EventPublisher) *TxRunner {
	return &TxRunner{pool: pool, events: events}
}

// writeRetryOpts retries a contended write (concurrent rotation, §9.1) on
// deadlock (40P01) / serialization failure (40001) with backoff + jitter.
var writeRetryOpts = pgcommon.RetryOptions{
	MaxAttempts:    3,
	InitialWait:    10 * time.Millisecond,
	MaxWait:        500 * time.Millisecond,
	Multiplier:     2.0,
	JitterFraction: 0.25,
}

// RunInTx runs fn inside a transaction, injects a tx-bound publisher into
// the ctx, and maps low-level connection errors into
// domain.ErrDBUnavailable (503) so handlers get a consistent 5xx shape
// (§17). SQL-level errors (unique violation, FK, check) bubble up
// unchanged for service-layer classification (§9.1 optimistic-lock
// conflict, §9.2 idempotency).
func (r *TxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return wrapConnErr(pgcommon.RunInTxWithRetryOpts(ctx, r.pool, pgx.TxOptions{}, writeRetryOpts, func(ctx context.Context, tx pgx.Tx) error {
		txCtx := port.WithTx(ctx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, r.events)
		}
		return fn(txCtx)
	}))
}

// WithTx re-exports port.WithTx for existing call sites in this package.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return port.WithTx(ctx, tx)
}

// TxFromContext re-exports port.TxFromContext for existing call sites in
// this package. New code should call port.TxFromContext directly.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	return port.TxFromContext(ctx)
}

// withPool runs fn inside a transaction, joining an existing one if present
// in ctx. Used by repository helpers so a read outside a service tx still
// binds RLS via pgcommon.RunInTx's checkout hook.
func withPool(ctx context.Context, pool *pgcommon.Pool, fn func(pgx.Tx) error) error {
	if tx, ok := port.TxFromContext(ctx); ok {
		return wrapConnErr(fn(tx))
	}
	return wrapConnErr(pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(_ context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts non-protocol database errors into
// domain.ErrDBUnavailable. SQL-protocol errors that are not
// connectivity/resource classes pass through so the service layer can
// distinguish an integrity violation from a network outage.
//
// SQLSTATE class 08 (connection exception) and 53 (insufficient
// resources) are classified via pgcommon helpers (v1.3.0, same as
// iam-user-profile / iam-org-membership). Class 57 (operator
// intervention) and 58 (system error) have no dedicated helper yet and
// are matched on the pgconn Error() text. puddle.ErrClosedPool is the
// other positively-identifiable connectivity failure. Transport-level
// IO/network errors that never reached Postgres (EOF, ECONNRESET, …)
// are also remapped so HTTP HandleError returns 503 rather than 500.
//
// Everything else — including a caller's own business error returned
// from inside RunInTx/withPool — passes through unchanged.
func wrapConnErr(err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if pgcommon.IsConnectionException(err) || pgcommon.IsInsufficientResources(err) || isOperatorOrSystemErrorSQLState(err) || errors.Is(err, puddle.ErrClosedPool) {
		return domain.NewError(domain.ErrDBUnavailable, "database unavailable")
	}
	if isNetworkError(err) {
		return domain.NewError(domain.ErrDBUnavailable, "database unavailable")
	}
	return err
}

// isNetworkError reports whether err is a Go-level network/IO failure that
// pgx surfaces when the TCP connection to Postgres is lost mid-flight.
// These never reach the SQLSTATE classification path because pgx never
// received a protocol response — they are unambiguously availability
// failures (503).
func isNetworkError(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	var sysErr syscall.Errno
	if errors.As(err, &sysErr) {
		switch sysErr { //nolint:exhaustive // only transient-connection codes are retryable
		case syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ETIMEDOUT:
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "EOF")
}

// isOperatorOrSystemErrorSQLState reports whether err is a Postgres error
// in SQLSTATE class 57 or 58. pgcommon v1.3.0 has dedicated helpers for
// 08/53 but not these two; we classify via the pgconn Error() text
// ("… (SQLSTATE 57P01)") so this package never imports pgconn — same
// pattern as iam-user-profile / iam-org-membership.
func isOperatorOrSystemErrorSQLState(err error) bool {
	if !pgcommon.IsPgError(err) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLSTATE 57") || strings.Contains(msg, "SQLSTATE 58")
}
