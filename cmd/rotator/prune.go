package main

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// pruneResult summarizes one maintenance-prune run (§4.2, §13.1).
type pruneResult struct {
	ProcessedEventsDeleted int
	OutboxDeleted          int64
}

// outboxPruner is the narrow capability runPrune needs from
// *outbox.Runner — defined locally (rather than taking the concrete type)
// so unit tests can fake it without a live Postgres-backed Runner.
// *outbox.Runner satisfies this interface structurally; main.go's call
// site needs no adaptation.
type outboxPruner interface {
	PrunePublished(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
}

// runPrune deletes processed_events rows older than ttlDays (batched,
// §4.2) and already-published outbox_events rows older than
// outboxOlderThan. Both are RLS-exempt operational tables (§4.3) — no
// tenant GUC needed.
func runPrune(ctx context.Context, processed port.ProcessedEventsStore, outboxRunner outboxPruner, ttlDays, limit int, outboxOlderThan time.Duration, log port.Logger) pruneResult {
	var result pruneResult

	n, err := processed.Prune(ctx, ttlDays, limit)
	if err != nil {
		logError(ctx, log, "prune: processed_events prune failed", nil, err)
	} else {
		result.ProcessedEventsDeleted = n
	}

	deleted, err := outboxRunner.PrunePublished(ctx, outboxOlderThan, limit)
	if err != nil {
		logError(ctx, log, "prune: outbox_events prune failed", nil, err)
	} else {
		result.OutboxDeleted = deleted
	}

	return result
}
