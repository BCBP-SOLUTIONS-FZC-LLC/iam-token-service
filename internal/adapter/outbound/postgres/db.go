package postgres

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	return cfg.DSN
}

// MigrationDSNFromEnv returns the DSN for schema migrations. Migrations
// MUST bypass PgBouncer because the migration runner uses
// pg_advisory_lock, which is session-scoped and breaks under transaction
// pooling (TS-CONFIG-2). MIGRATION_DATABASE_URL must be set whenever
// PG_BOUNCER_MODE=true.
func MigrationDSNFromEnv() string {
	if dsn := os.Getenv("MIGRATION_DATABASE_URL"); dsn != "" {
		return dsn
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
		return dsn
	}
	return DSNFromEnv()
}

// SystemPoolName is the "pool" label of the BYPASSRLS reconciler pool on
// platform-pgcommon's platform_db_* metrics; the app pool keeps
// ConfigFromEnv's PG_POOL_NAME (default "default").
const SystemPoolName = "reconciler"

// SystemPoolConfig returns pgcommon.Config for the BYPASSRLS reconciler
// pool (§4.3/RLS-7), matching iam-user-profile / iam-org-membership.
// The pool deliberately has no GUCProvider — cross-tenant rotator /
// overlap-exporter queries run under serviceaccount_reconciler — but
// still connects through PgBouncer in production, so PGBouncerMode is
// forced true (SimpleProtocol + MinConns:0). A bare
// pgcommon.Config{DSN, Logger} literal would leave PGBouncerMode at the
// Go zero-value false and drop ConfigFromEnv pool sizing.
//
// Pool sizing, lifetimes, SlowQueryThreshold and the per-transaction
// StatementTimeout / LockTimeout (PG_STATEMENT_TIMEOUT / PG_LOCK_TIMEOUT,
// applied by pgcommon with SET LOCAL) are copied from ConfigFromEnv so the
// reconciler pool and the app pool share one env-driven source of truth. Tracer is left unset — call sites wire
// gincommon.NewSpanTracer so db.query spans export through gincommon's
// TracerProvider.
func SystemPoolConfig(dsn string, log port.Logger) pgcommon.Config {
	cfg, _ := pgcommon.ConfigFromEnv()
	cfg.DSN = dsn
	cfg.GUCProvider = nil
	cfg.PGBouncerMode = true
	cfg.Tracer = nil
	// Its own "pool" label on the platform_db_* metrics: NewPool sums the
	// connection gauges of live pools sharing a PoolName, which would blend
	// this BYPASSRLS pool into the app pool's utilisation.
	cfg.PoolName = SystemPoolName
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
	return wrapConnErrCtx(ctx, pgcommon.RunInTxWithRetryOpts(ctx, r.pool, pgx.TxOptions{}, writeRetryOpts, func(ctx context.Context, tx pgx.Tx) error {
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
		return wrapConnErrCtx(ctx, fn(tx))
	}
	return wrapConnErrCtx(ctx, pgcommon.RunInTx(ctx, pool, pgx.TxOptions{}, func(_ context.Context, tx pgx.Tx) error {
		return fn(tx)
	}))
}

// wrapConnErr converts non-protocol database errors into
// domain.ErrDBUnavailable. SQL-protocol errors that are not
// connectivity/resource classes pass through so the service layer can
// distinguish an integrity violation from a network outage.
//
// isUnavailableSQLState classifies by the *pgconn.PgError's SQLSTATE class
// (Code[:2]): 08 connection exception, 53 insufficient resources, 57
// operator intervention (57014 query_canceled only while the request's own
// context is still live — see wrapConnErrCtx), 58 system error.
// puddle.ErrClosedPool is the other positively-identifiable connectivity
// failure. 55P03 lock_not_available is mapped separately, to
// domain.ErrRotationInFlight (TS-D22).
//
// Everything else — including a caller's own business error returned
// from inside RunInTx/withPool, and a transport-level failure that never
// reached Postgres — passes through unchanged. Defaulting unrecognized
// errors to ErrDBUnavailable (as this used to, via a hand-rolled
// isNetworkError remap) silently discarded the caller's real error under
// a misleading "database unavailable" 503 — the bug fixed in
// iam-user-profile / iam-org-membership. HTTP HandleError independently
// classifies a leaked PgError of these same connectivity/resource
// classes into 503 (and 55P03 into 409).
func wrapConnErr(err error) error {
	return wrapConnErrCtx(context.Background(), err)
}

// wrapConnErrCtx is wrapConnErr for a call made under ctx. It also maps
// SQLSTATE 55P03 to domain.ErrRotationInFlight (errLockNotAvailable). SQLSTATE 57014
// (query_canceled) covers both a server-side statement_timeout — a genuine
// "database too slow" condition, classified db_unavailable — and pgx
// cancelling the statement because ctx ended (client disconnect, request
// deadline). The latter is not a database outage: the error is returned
// unchanged so it is never counted as db_unavailable.
func wrapConnErrCtx(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return err
	}
	if isQueryCanceled(err) && ctx.Err() != nil {
		return err
	}
	if isLockNotAvailable(err) {
		return errLockNotAvailable()
	}
	if isUnavailableSQLState(err) || errors.Is(err, puddle.ErrClosedPool) {
		return domain.NewError(domain.ErrDBUnavailable, "database unavailable")
	}
	return err
}

// errLockNotAvailable is the one mapping of SQLSTATE 55P03
// (lock_not_available — a row-lock wait exceeded the transaction's
// PG_LOCK_TIMEOUT) for the whole adapter: another credential write holds
// the principal row (TS-1/TS-2 under LockForUpdate, the offboarding
// cascade under LockByTenant), so the caller gets the retryable 409
// rotation_in_flight (§17) rather than a raw PgError that becomes a 500.
// Centralised here because a lock wait can surface from any statement that
// touches a locked row (TS-4's ON CONFLICT DO UPDATE, a FOR UPDATE query)
// and — with pgx v5 — from QueryRow.Scan or rows.Err(), not from Query.
func errLockNotAvailable() error {
	return domain.NewError(domain.ErrRotationInFlight, "another credential write for this principal is in progress; retry")
}

// isLockNotAvailable reports SQLSTATE 55P03 (lock_not_available).
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// isUnavailableSQLState positively identifies a database-availability
// failure by SQLSTATE class (never by a broad "looks like a network error"
// heuristic, TS-D16): 08 connection exception, 53 insufficient resources,
// 57 operator intervention (incl. 57014 statement_timeout), 58 system error.
func isUnavailableSQLState(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "08", "53", "57", "58":
		return true
	}
	return false
}

// isQueryCanceled reports SQLSTATE 57014 (query_canceled).
func isQueryCanceled(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}
