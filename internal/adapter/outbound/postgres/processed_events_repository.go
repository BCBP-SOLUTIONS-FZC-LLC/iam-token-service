package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// ProcessedEventsRepository implements the offboarding-consumer dedup
// ledger (§4.2, §7.1) against `processed_events`. RLS-exempt — this table
// carries no tenant_id column.
type ProcessedEventsRepository struct {
	pool *pgcommon.Pool
}

// NewProcessedEventsRepository constructs a ProcessedEventsRepository
// backed by pool.
func NewProcessedEventsRepository(pool *pgcommon.Pool) *ProcessedEventsRepository {
	return &ProcessedEventsRepository{pool: pool}
}

var _ port.ProcessedEventsStore = (*ProcessedEventsRepository)(nil)

// IsProcessed reports whether eventID has already been recorded as
// processed for consumer.
func (r *ProcessedEventsRepository) IsProcessed(ctx context.Context, consumer port.ProcessedEventsConsumer, eventID string) (bool, error) {
	var exists bool
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1 AND consumer = $2)`,
			eventID, string(consumer),
		).Scan(&exists)
	})
	if err != nil {
		return false, err
	}
	return exists, nil
}

// MarkProcessed records eventID as processed for consumer, idempotently
// (ON CONFLICT DO NOTHING — the PK serializes duplicate deliveries at the
// DB level, §4.2). Called inside TxRunner.RunInTx, this join the same
// transaction as the offboarding cascade's writes (§9.2), so dedup and the
// cascade commit atomically.
func (r *ProcessedEventsRepository) MarkProcessed(ctx context.Context, consumer port.ProcessedEventsConsumer, eventID string) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer)
			VALUES ($1, $2)
			ON CONFLICT (event_id, consumer) DO NOTHING`, eventID, string(consumer))
		return err
	})
}

// Prune deletes up to limit rows older than ttlDays (§4.2/§15.4). Batched
// so a backlog larger than one tick converges over several ticks instead
// of one unbounded DELETE.
func (r *ProcessedEventsRepository) Prune(ctx context.Context, ttlDays, limit int) (int, error) {
	var n int
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM processed_events
			WHERE (event_id, consumer) IN (
				SELECT event_id, consumer FROM processed_events
				WHERE processed_at < now() - make_interval(days => $1)
				LIMIT $2
			)`, ttlDays, limit)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		return nil
	})
	return n, err
}
