package port

import (
	"context"
	"time"
)

// ProcessedEventsConsumer names a consumer's rows in `processed_events`
// (composite PK with event_id, §4.2). Frozen value for this service's one
// inbound subscription (§25): 'tenant_offboarding'.
type ProcessedEventsConsumer string

// ProcessedEventsConsumerTenantOffboarding is the consumer name for the
// TenantMembershipsPurged offboarding cascade (§7.1, frozen §25).
const ProcessedEventsConsumerTenantOffboarding ProcessedEventsConsumer = "tenant_offboarding"

// Inbox is the exactly-once consumer port backing the processed_events
// table (§4.2, §9.2), implemented over platform-events' pkg/inbox — the
// same port iam-org-membership uses.
//
// ProcessOnce claims (consumer, eventID) and runs fn in ONE transaction:
// the claim (INSERT … ON CONFLICT DO NOTHING) is the first statement, so a
// concurrent delivery of the same event blocks on the claim's row lock and
// then sees it as a duplicate, instead of both deliveries passing a
// pre-check and running the cascade twice. fn's ctx carries that
// transaction (and the tx-bound event publisher), so every repository call
// and outbox enqueue inside fn joins it; the claim and fn's writes commit or
// roll back together. If fn returns an error nothing is recorded and the
// message is retried. The transaction runs on the RLS-scoped app pool, so a
// pgcommon.GUCSet in ctx is bound with SET LOCAL as on every other path
// (RLS-6).
//
// duplicate is true when the event was already processed; fn was not run.
// eventID must be a UUID (every platform-events envelope ID is).
type Inbox interface {
	ProcessOnce(ctx context.Context, consumer ProcessedEventsConsumer, eventID, eventType string, fn func(txCtx context.Context) error) (duplicate bool, err error)
}

// InboxPruner deletes one consumer's processed_events rows older than
// retention, in batches of batch rows until none are left (§4.2/§15.4).
type InboxPruner interface {
	Prune(ctx context.Context, consumer ProcessedEventsConsumer, retention time.Duration, batch int) (int64, error)
}
