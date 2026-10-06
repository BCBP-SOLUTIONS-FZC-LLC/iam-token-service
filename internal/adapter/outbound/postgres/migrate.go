// Package postgres implements the outbound repository ports backed by
// PostgreSQL through platform-pgcommon.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	pgmigrate "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/migrate"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// migrationsFS embeds every .sql file under migrations/. Files are numbered
// NNNNNN_<name>.up.sql / .down.sql (golang-migrate convention, §4.4/§19).
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// MigrationsFS returns the embedded domain migrations rooted at their
// directory — what a pgmigrate.Runner's FS expects. Exported for the
// up/down/up round-trip test (test/postgres), which needs Runner.Down; the
// service itself only ever migrates up (RunMigrations).
func MigrationsFS() fs.FS {
	// fs.Sub on an embedded FS with a known directory path is infallible.
	sub, _ := fs.Sub(migrationsFS, "migrations") //nolint:errcheck // infallible, see comment above
	return sub
}

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
	runner := &pgmigrate.Runner{FS: MigrationsFS(), DSN: dsn}
	if len(log) > 0 && log[0] != nil {
		runner.Logger = NewLoggerAdapter(log[0])
	}
	return runner.Up(ctx)
}

// Migrate applies platform-events' outbox schema, then this service's
// domain migrations — that order is required (MIG-2: the domain migration
// ALTERs outbox_events). dsn must bypass PgBouncer (TS-CONFIG-2).
//
// A database that is already AHEAD of this binary (pgmigrate's
// ErrVersionNotInSource — the state an image rollback leaves after a forward
// migration ran) is logged and tolerated rather than fatal: migrations are
// additive (expand/contract, §4.4), so the older binary runs against the
// newer schema, and failing here would turn every rollback — including
// release.yml's automatic one — into a crashloop.
func Migrate(ctx context.Context, dsn string, log port.Logger) error {
	if err := outbox.ApplySchema(ctx, &pgmigrate.Runner{DSN: dsn}); err != nil {
		if !tolerateNewerSchema(err, "outbox schema", log) {
			return fmt.Errorf("outbox schema: %w", err)
		}
	}
	if err := RunMigrations(ctx, dsn, log); err != nil {
		if !tolerateNewerSchema(err, "domain migrations", log) {
			return fmt.Errorf("domain migrations: %w", err)
		}
	}
	return nil
}

func tolerateNewerSchema(err error, step string, log port.Logger) bool {
	if !errors.Is(err, pgmigrate.ErrVersionNotInSource) {
		return false
	}
	if log != nil {
		log.Warn("database schema is newer than this binary's migrations — continuing (image rollback?)", map[string]interface{}{
			"step": step, "error": err.Error(),
		})
	}
	return true
}

// MigrationsEnabledFromEnv reports RUN_MIGRATIONS (default true). Helm sets
// it to "false" on every Deployment and CronJob: migrations run once per
// release in the pre-install/pre-upgrade migrate Job instead, so long-running
// pods never hold the BYPASSRLS migrator DSN. Local runs and compose keep the
// default and migrate at startup.
func MigrationsEnabledFromEnv() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("RUN_MIGRATIONS")), "false")
}

// MigrateOnlyFromEnv reports MIGRATE_ONLY: the server binary applies the
// migrations and exits 0 without starting anything else (the Helm migrate
// Job's mode).
func MigrateOnlyFromEnv() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("MIGRATE_ONLY")), "true")
}
