package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// RLSViolationRepository reads and prunes rls_violation_log (migration
// 000003), the sampled audit trail rls_check_tenant() writes when an RLS
// check fails. It backs the Tier 2 iam_rls_violations_total counter.
//
// CountSince/CursorBefore need the BYPASSRLS reconciler pool (the table is
// only granted to serviceaccount_reconciler and admin_readonly); Prune
// needs the app pool (prune_rls_violation_log() is granted to
// serviceaccount_app). Like ReconcilerRepository, it implements no core
// port: its only callers are cmd/server's exporter and cmd/rotator's prune.
type RLSViolationRepository struct {
	pool *pgcommon.Pool
}

// NewRLSViolationRepository constructs an RLSViolationRepository over pool.
func NewRLSViolationRepository(pool *pgcommon.Pool) *RLSViolationRepository {
	return &RLSViolationRepository{pool: pool}
}

// CountSince returns the count of logged violations by violation_type with
// id > afterID, and the highest id seen (afterID when there are none) for
// the caller's next cursor. Rows younger than countLag are left for the next
// call: bigserial ids are assigned at INSERT but become visible at COMMIT,
// so a row from a transaction that commits late can carry an id below one
// already counted — the lag lets such stragglers land before the cursor
// moves past them.
func (r *RLSViolationRepository) CountSince(ctx context.Context, afterID int64) (map[string]int64, int64, error) {
	counts := map[string]int64{}
	lastID := afterID
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT violation_type, count(*), max(id) FROM rls_violation_log
			 WHERE id > $1 AND occurred_at < now() - make_interval(secs => $2)
			 GROUP BY violation_type`, afterID, countLag.Seconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var vType string
			var n, maxID int64
			if err := rows.Scan(&vType, &n, &maxID); err != nil {
				return err
			}
			counts[vType] = n
			lastID = max(lastID, maxID)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, afterID, err
	}
	return counts, lastID, nil
}

// countLag is how long a row must have existed before CountSince counts it.
const countLag = 5 * time.Second

// CursorBefore returns the highest id logged before now-window (0 when
// none): the exporter's starting cursor, so a restarted pod counts only the
// last window instead of re-adding the whole table's history.
func (r *RLSViolationRepository) CursorBefore(ctx context.Context, window time.Duration) (int64, error) {
	var id int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT COALESCE(max(id), 0) FROM rls_violation_log
			 WHERE occurred_at < now() - make_interval(secs => $1)`, window.Seconds()).Scan(&id)
	})
	return id, err
}

// Prune deletes up to limit rows older than ttlDays through
// prune_rls_violation_log() and returns how many it deleted.
func (r *RLSViolationRepository) Prune(ctx context.Context, ttlDays, limit int) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT prune_rls_violation_log($1, $2)`, ttlDays, limit).Scan(&n)
	})
	return n, err
}
