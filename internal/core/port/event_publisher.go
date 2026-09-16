package port

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
)

// EventPublisher enqueues events into the transactional outbox. The insert
// MUST share the same transaction as the business write so state and event
// commit atomically (EVT-1, TS-INV-5).
//
// Callers never pass a pgx.Tx. TxRunner.RunInTx stores the open transaction
// on ctx; the eventbus adapter reads it via port.TxFromContext and writes
// outbox_events on that tx.
type EventPublisher interface {
	Enqueue(ctx context.Context, event *domain.Event) error
}
