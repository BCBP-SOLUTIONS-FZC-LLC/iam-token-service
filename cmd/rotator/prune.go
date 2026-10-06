package main

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// pruneResult summarizes one maintenance-prune run (§4.2, §13.1).
type pruneResult struct {
	ProcessedEventsDeleted int64
	OutboxDeleted          int64
	Failed                 int // prune steps that errored — they fail the run (exit 1)
	Deferred               int // prune steps not started or cut off by the run budget/signal — the next run continues
}

// pruneReserve is the run time a prune step needs before it starts. Each
// prune deletes in batches of its own transaction, so a step the run
// deadline (or SIGTERM) cuts off mid-way loses at most the in-flight batch,
// which rolls back: that is Deferred, not a failure.
const pruneReserve = 5 * time.Second

// pruneOutcome classifies one prune step's error: nil → ok; an error after
// the run's budget ran out or the run was signalled → deferred (the
// batches already committed stay deleted; the next run continues);
// anything else → failed.
func pruneOutcome(ctx context.Context, err error) (deferred, failed bool) {
	switch {
	case err == nil:
		return false, false
	case ctx.Err() != nil:
		return true, false
	default:
		return false, true
	}
}

// outboxPruner is the narrow capability runPrune needs from
// *outbox.Runner — defined locally (rather than taking the concrete type)
// so unit tests can fake it without a live Postgres-backed Runner.
// *outbox.Runner satisfies this interface structurally; main.go's call
// site needs no adaptation.
type outboxPruner interface {
	PrunePublished(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
}

// runPrune deletes the tenant_offboarding consumer's processed_events rows
// older than ttlDays through platform-events' inbox (batched until none are
// left, §4.2) and already-published outbox_events rows older than
// outboxOlderThan. Both are RLS-exempt operational tables (§4.3) — no
// tenant GUC needed.
func runPrune(ctx context.Context, processed port.InboxPruner, outboxRunner outboxPruner, ttlDays, limit int, outboxOlderThan time.Duration, log port.Logger) pruneResult {
	var result pruneResult

	if stopStarting(ctx, pruneReserve) {
		result.Deferred++
	} else {
		n, err := processed.Prune(ctx, port.ProcessedEventsConsumerTenantOffboarding, time.Duration(ttlDays)*24*time.Hour, limit)
		result.ProcessedEventsDeleted = n
		switch deferred, failed := pruneOutcome(ctx, err); {
		case deferred:
			result.Deferred++
		case failed:
			result.Failed++
			result.ProcessedEventsDeleted = 0
			logError(ctx, log, "prune: processed_events prune failed", nil, err)
		}
	}

	if stopStarting(ctx, pruneReserve) {
		result.Deferred++
	} else {
		deleted, err := outboxRunner.PrunePublished(ctx, outboxOlderThan, limit)
		switch deferred, failed := pruneOutcome(ctx, err); {
		case deferred:
			result.Deferred++
		case failed:
			result.Failed++
			logError(ctx, log, "prune: outbox_events prune failed", nil, err)
		default:
			result.OutboxDeleted = deleted
		}
	}

	return result
}

// rlsViolationPruner is the capability runRLSViolationPrune needs from
// pgadapter.RLSViolationRepository (built over the app pool, which may only
// call prune_rls_violation_log(), never DELETE the audit table directly).
type rlsViolationPruner interface {
	Prune(ctx context.Context, ttlDays, limit int) (int, error)
}

// runRLSViolationPrune deletes rls_violation_log rows older than ttlDays
// (migration 000003), one batch of limit per call, repeated while a full
// batch came back and the run budget allows. A failure is logged and
// reported (failed=true) so the run exits non-zero — a prune that never
// succeeds is a misconfiguration worth surfacing, even though one missed
// run costs nothing. Running out of budget (or SIGTERM) is not a failure.
func runRLSViolationPrune(ctx context.Context, pruner rlsViolationPruner, ttlDays, limit int, log port.Logger) (deleted int, failed bool) {
	for !stopStarting(ctx, pruneReserve) {
		n, err := pruner.Prune(ctx, ttlDays, limit)
		if deferred, failedNow := pruneOutcome(ctx, err); deferred {
			return deleted, false
		} else if failedNow {
			logError(ctx, log, "prune: rls_violation_log prune failed", nil, err)
			return 0, true
		}
		deleted += n
		if n < limit {
			break
		}
	}
	return deleted, false
}
