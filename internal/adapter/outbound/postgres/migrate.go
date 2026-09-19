// Package postgres implements the outbound repository ports backed by
// PostgreSQL through platform-pgcommon.
package postgres

import (
	"context"
	"embed"
	"io/fs"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/migrate"
)

// migrationsFS embeds every .sql file under migrations/. Files are numbered
// NNNNNN_<name>.up.sql / .down.sql (golang-migrate convention, §4.4/§19).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// RunMigrations applies all pending domain migrations against dsn. Uses the
// direct Postgres DSN (bypassing PgBouncer) because the runner acquires a
// pg_advisory_lock which is session-scoped (§4.4/TS-CONFIG-2). MUST run
// AFTER outbox.ApplySchema (MIG-2 — the domain migration ALTERs
// outbox_events, which must already exist).
//
// log is optional (variadic so existing call sites keep compiling) — when
// provided, each migration step is logged through it via LoggerAdapter
// instead of going nowhere (pgmigrate.Runner.Logger, same as
// iam-user-profile / iam-org-membership).
func RunMigrations(ctx context.Context, dsn string, log ...port.Logger) error {
	// fs.Sub on an embedded FS with a known directory path is infallible.
	sub, _ := fs.Sub(migrationsFS, "migrations") //nolint:errcheck // infallible, see comment above
	runner := &pgmigrate.Runner{FS: sub, DSN: dsn}
	if len(log) > 0 && log[0] != nil {
		runner.Logger = NewLoggerAdapter(log[0])
	}
	return runner.Up(ctx)
}
