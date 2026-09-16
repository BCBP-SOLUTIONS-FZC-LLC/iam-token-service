package port

import "context"

// ProcessedEventsConsumer names a consumer's row in `processed_events`
// (composite PK with event_id, §4.2). Frozen value for this service's one
// inbound subscription (§25): 'tenant_offboarding'.
type ProcessedEventsConsumer string

// ProcessedEventsConsumerTenantOffboarding is the consumer name for the
// TenantMembershipsPurged offboarding cascade (§7.1, frozen §25).
const ProcessedEventsConsumerTenantOffboarding ProcessedEventsConsumer = "tenant_offboarding"

// ProcessedEventsStore implements the inbound-consumer dedup ledger (§4.2,
// §7.1, §9.2). platform-events has no processed-events API of its own —
// consumers implement Envelope.ID + INSERT ON CONFLICT DO NOTHING against
// a local table, matching iam-user-profile and iam-org-membership.
type ProcessedEventsStore interface {
	IsProcessed(ctx context.Context, consumer ProcessedEventsConsumer, eventID string) (bool, error)
	MarkProcessed(ctx context.Context, consumer ProcessedEventsConsumer, eventID string) error
	// Prune deletes up to limit rows older than ttlDays (§4.2/§15.4).
	// Batched so a backlog larger than one tick converges over several
	// ticks instead of one unbounded DELETE.
	Prune(ctx context.Context, ttlDays, limit int) (int, error)
}
