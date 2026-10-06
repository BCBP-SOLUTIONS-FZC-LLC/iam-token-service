package postgres

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/inbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// InboxRepository implements port.Inbox and port.InboxPruner with
// platform-events' inbox.Store, one Store per consumer name over the
// service-owned processed_events table (§4.2; RLS-exempt). Matches
// iam-org-membership's InboxRepository. See port.Inbox for the transaction
// contract.
type InboxRepository struct {
	pool   *pgcommon.Pool
	events port.EventPublisher

	mu     sync.Mutex
	stores map[port.ProcessedEventsConsumer]*inbox.Store
}

var (
	_ port.Inbox       = (*InboxRepository)(nil)
	_ port.InboxPruner = (*InboxRepository)(nil)
)

// NewInboxRepository builds an InboxRepository on pool. events is bound into
// fn's ctx (port.WithEventPublisher) exactly as TxRunner does, so a consumer
// can enqueue outbox events atomically with its writes; nil disables it.
func NewInboxRepository(pool *pgcommon.Pool, events port.EventPublisher) *InboxRepository {
	return &InboxRepository{pool: pool, events: events, stores: map[port.ProcessedEventsConsumer]*inbox.Store{}}
}

// ProcessOnce implements port.Inbox.
func (r *InboxRepository) ProcessOnce(ctx context.Context, consumer port.ProcessedEventsConsumer, eventID, eventType string, fn func(txCtx context.Context) error) (bool, error) {
	store, err := r.store(consumer)
	if err != nil {
		return false, err
	}
	ran := false
	env := events.Envelope[json.RawMessage]{ID: eventID, Type: eventType}
	err = store.Process(ctx, env, func(ctx context.Context, tx pgcommon.Tx) error {
		ran = true
		txCtx := port.WithTx(ctx, tx)
		if r.events != nil {
			txCtx = port.WithEventPublisher(txCtx, r.events)
		}
		return fn(txCtx)
	})
	if err != nil {
		return false, wrapConnErrCtx(ctx, err)
	}
	return !ran, nil
}

// Prune implements port.InboxPruner with inbox.Store.Prune, which loops
// batch-sized deletes until no expired row is left.
func (r *InboxRepository) Prune(ctx context.Context, consumer port.ProcessedEventsConsumer, retention time.Duration, batch int) (int64, error) {
	store, err := r.store(consumer)
	if err != nil {
		return 0, err
	}
	n, err := store.Prune(ctx, retention, batch)
	return n, wrapConnErrCtx(ctx, err)
}

func (r *InboxRepository) store(consumer port.ProcessedEventsConsumer) (*inbox.Store, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.stores[consumer]; ok {
		return s, nil
	}
	s, err := inbox.NewStore(r.pool, string(consumer))
	if err != nil {
		return nil, err
	}
	r.stores[consumer] = s
	return s, nil
}
