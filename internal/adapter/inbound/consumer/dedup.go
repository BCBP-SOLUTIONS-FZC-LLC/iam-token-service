package consumer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// queueNameForConsumer maps this service's small, fixed set of
// ProcessedEventsConsumer values to the SQS queue they read from — the
// `queue` label on the Tier-1 platform_duplicate_messages_total (Enterprise
// Platform Observability Standard's approved dimension for this metric).
// Bounded cardinality: one entry per consumer this service ever wires, not
// a pass-through of unbounded caller input.
func queueNameForConsumer(consumer port.ProcessedEventsConsumer) string {
	if consumer == port.ProcessedEventsConsumerTenantOffboarding {
		return "tenant-lifecycle-tokensvc-q"
	}
	return string(consumer)
}

// skipDuplicate is the cheap processed_events probe every known-type
// handler runs before doing work. A hit increments both the legacy
// iam_token_service_processed_events_duplicates_total and the
// registry-proposed platform_duplicate_messages_total (dual-emitted during
// the compatibility period), then short-circuits. platform-events has no
// processed-events API — Envelope.ID + INSERT ON CONFLICT DO NOTHING is the
// library's documented consumer pattern (same as iam-user-profile /
// iam-org-membership).
func skipDuplicate(ctx context.Context, dedup port.ProcessedEventsStore, consumer port.ProcessedEventsConsumer, eventID string) (bool, error) {
	processed, err := dedup.IsProcessed(ctx, consumer, eventID)
	if err != nil {
		return false, err
	}
	if processed {
		if metrics.ProcessedEventsDuplicates != nil {
			metrics.ProcessedEventsDuplicates.WithLabelValues(string(consumer)).Inc()
		}
		if metrics.DuplicateMessagesTotal != nil {
			metrics.DuplicateMessagesTotal.WithLabelValues(queueNameForConsumer(consumer)).Inc()
		}
		return true, nil
	}
	return false, nil
}

// ackUnknown is the forward-compat path: an event type with no wired
// handler is logged, counted, and recorded in processed_events inside a
// RunInTx so redelivery does not storm the same unknown type.
func ackUnknown(ctx context.Context, tx port.TxRunner, dedup port.ProcessedEventsStore, log port.Logger, consumer port.ProcessedEventsConsumer, env events.Envelope[json.RawMessage]) error {
	if metrics.UnknownEventAcknowledged != nil {
		metrics.UnknownEventAcknowledged.WithLabelValues(string(consumer), env.Type).Inc()
	}
	if log != nil {
		log.Info("unknown event type — silently acknowledging", map[string]interface{}{
			"event_type": env.Type, "event_id": env.ID, "consumer": string(consumer),
		})
	}
	return markProcessedInTx(ctx, tx, dedup, consumer, env.ID)
}

// markProcessedInTx records eventID on the caller's TxRunner so the dedup
// insert joins withPool's ambient transaction (or opens its own when the
// handler has no local write to be atomic with).
func markProcessedInTx(ctx context.Context, tx port.TxRunner, dedup port.ProcessedEventsStore, consumer port.ProcessedEventsConsumer, eventID string) error {
	return tx.RunInTx(ctx, func(txCtx context.Context) error {
		if err := dedup.MarkProcessed(txCtx, consumer, eventID); err != nil {
			return fmt.Errorf("mark processed: %w", err)
		}
		return nil
	})
}
