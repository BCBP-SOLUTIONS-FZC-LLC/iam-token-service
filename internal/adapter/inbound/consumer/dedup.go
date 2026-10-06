package consumer

import (
	"context"
	"encoding/json"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// processOnce runs fn exactly once per (consumer, env.ID) through the
// platform-events inbox (port.Inbox): the processed_events claim and fn's
// writes commit in one transaction (§9.2), matching iam-org-membership. A
// duplicate skips fn and increments iam_token_service_processed_events_duplicates_total,
// this repo's authoritative signal; the inbox itself records the Tier-1
// platform_duplicate_messages_total{queue,event_type}.
func processOnce(ctx context.Context, inbox port.Inbox, consumer port.ProcessedEventsConsumer, env events.Envelope[json.RawMessage], fn func(txCtx context.Context) error) (bool, error) {
	duplicate, err := inbox.ProcessOnce(ctx, consumer, env.ID, env.Type, fn)
	if err != nil {
		return false, err
	}
	if duplicate && metrics.ProcessedEventsDuplicates != nil {
		metrics.ProcessedEventsDuplicates.WithLabelValues(string(consumer)).Inc()
	}
	return duplicate, nil
}

// knownEventTypes is the closed set of tenant-lifecycle event types that
// may legitimately reach tenant-lifecycle-tokensvc-q (iam-org-membership's
// tenant-lifecycle topic, plus the pre-ADR-0008 name). The event type comes
// from the producer, so a shape check alone (the previous regexp) still let
// any well-formed string mint a new series; anything outside this set is
// folded into "other" to keep the metric's cardinality fixed.
var knownEventTypes = map[string]struct{}{
	tenantMembershipsPurgedEventType: {},
	"TenantCreated":                  {},
	"TenantStateChanged":             {},
	"TenantRoleGranted":              {},
	"TenantRoleRevoked":              {},
	"TenantSeatOverageStarted":       {},
	"TenantSeatOverageResolved":      {},
	"TenantOffboarded":               {},
}

func boundedEventType(eventType string) string {
	if _, ok := knownEventTypes[eventType]; ok {
		return eventType
	}
	return "other"
}

// ackUnknown is the forward-compat path: an event type with no wired
// handler is logged, counted, and recorded in processed_events so
// redelivery does not storm the same unknown type.
func ackUnknown(ctx context.Context, inbox port.Inbox, log port.Logger, consumer port.ProcessedEventsConsumer, env events.Envelope[json.RawMessage]) error {
	if metrics.UnknownEventAcknowledged != nil {
		metrics.UnknownEventAcknowledged.WithLabelValues(string(consumer), boundedEventType(env.Type)).Inc()
	}
	if log != nil {
		log.Info("unknown event type — silently acknowledging", withTraceID(ctx, map[string]interface{}{
			"event_type": env.Type, "event_id": env.ID, "consumer": string(consumer),
		}))
	}
	// The inbox uses the type only as platform_duplicate_messages_total's
	// event_type label, so it gets the same bounded value: a redelivered
	// unknown type must not mint a new Tier-1 series either.
	bounded := env
	bounded.Type = boundedEventType(env.Type)
	_, err := processOnce(ctx, inbox, consumer, bounded, func(context.Context) error { return nil })
	return err
}
