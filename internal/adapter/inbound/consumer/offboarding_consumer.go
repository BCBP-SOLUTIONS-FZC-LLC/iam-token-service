// Package consumer implements this service's one inbound SQS subscription:
// TenantMembershipsPurged on tenant-lifecycle-tokensvc-q, driving the
// tenant-offboarding cascade (§7.1, §8.4).
package consumer

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// tenantMembershipsPurgedEventType is the wire-format `type` field on a
// TenantMembershipsPurged envelope, published by iam-org-membership onto
// the tenant-lifecycle queue this service consumes from.
const tenantMembershipsPurgedEventType = "TenantMembershipsPurged"

// OffboardingConsumer implements events.Handler for TenantMembershipsPurged
// (§8.4): hard-deletes the tenant's service_account_* rows and every
// credential's OpenBao material, then emits ServiceAccountRevoked per
// principal found. Idempotent via processed_events
// (consumer='tenant_offboarding'), keyed on the envelope id (§9.2).
// Unknown types are silently acked (iam-user-profile / iam-org-membership
// ackUnknown) so a producer schema addition does not DLQ-storm.
type OffboardingConsumer struct {
	principals  port.PrincipalRepository
	credentials port.CredentialRepository
	secrets     port.SecretStore
	processed   port.ProcessedEventsStore
	tx          port.TxRunner
	log         port.Logger
}

// NewOffboardingConsumer wires OffboardingConsumer's dependencies.
func NewOffboardingConsumer(
	principals port.PrincipalRepository,
	credentials port.CredentialRepository,
	secrets port.SecretStore,
	processed port.ProcessedEventsStore,
	tx port.TxRunner,
	log port.Logger,
) *OffboardingConsumer {
	return &OffboardingConsumer{
		principals: principals, credentials: credentials, secrets: secrets,
		processed: processed, tx: tx, log: log,
	}
}

// tenantMembershipsPurgedPayload extracts only the field this consumer
// needs. The canonical schema for TenantMembershipsPurged is owned by its
// producing service, not this repo (§7.1) — additional fields are ignored,
// not validated.
type tenantMembershipsPurgedPayload struct {
	TenantID uuid.UUID `json:"tenant_id"`
}

// Handle implements events.Handler. The span is started on gincommon's
// process-global TracerProvider (cmd/consumer's InitTracingFromEnv) so
// offboarding work joins the same OTLP pipeline as HTTP/db spans.
func (c *OffboardingConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	ctx, span := otel.Tracer("iam-token-service").Start(ctx, "consumer.offboarding")
	defer span.End()

	if env.ID == "" {
		c.error(ctx, "envelope has invalid or missing id — cannot dedup, acknowledging to prevent redelivery loop", map[string]interface{}{
			"event_type": env.Type,
		})
		return nil
	}
	if _, err := uuid.Parse(env.ID); err != nil {
		c.error(ctx, "envelope has invalid or missing id — cannot dedup, acknowledging to prevent redelivery loop", map[string]interface{}{
			"event_type": env.Type, "event_id": env.ID,
		})
		return nil
	}

	if env.Type != tenantMembershipsPurgedEventType {
		return ackUnknown(ctx, c.tx, c.processed, c.log, port.ProcessedEventsConsumerTenantOffboarding, env)
	}

	// Idempotency short-circuit (§9.2, §17 'duplicate' disposition) —
	// processed_events is RLS-exempt, no tenant GUC needed for this read.
	seen, err := skipDuplicate(ctx, c.processed, port.ProcessedEventsConsumerTenantOffboarding, env.ID)
	if err != nil {
		return err
	}
	if seen {
		c.debug(ctx, "offboarding event already processed, skipping", env.ID)
		return nil
	}

	var payload tenantMembershipsPurgedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("offboarding consumer: decode TenantMembershipsPurged payload: %w", err)
	}
	if payload.TenantID == uuid.Nil {
		return fmt.Errorf("offboarding consumer: TenantMembershipsPurged payload missing tenant_id")
	}

	// Bind the RLS GUC to this event's tenant + the reserved system
	// principal (§4.3) — this consumer processes exactly one tenant per
	// message, so it uses the normal RLS-scoped serviceaccount_app path,
	// never BYPASSRLS (that is reserved for cmd/rotator's cross-tenant
	// reconciler jobs, RLS-7).
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = domain.SystemPrincipalID.String()
	g.TenantID = payload.TenantID.String()
	ctx = pgcommon.WithGUCSet(ctx, g)

	principals, err := c.principals.ListByTenant(ctx, payload.TenantID)
	if err != nil {
		return err
	}

	// Collect every credential's OpenBao path across every principal
	// BEFORE any delete, so material is still discoverable even if this
	// process crashes mid-cascade.
	var paths []string
	for _, p := range principals {
		creds, err := c.credentials.ListByPrincipal(ctx, payload.TenantID, p.ID)
		if err != nil {
			return err
		}
		for _, cred := range creds {
			paths = append(paths, cred.OpenBaoPath)
		}
	}

	// OpenBao material deleted BEFORE the Postgres commit — the same
	// material-first discipline TS-2 revoke uses in the destructive
	// direction (§9.3). This ordering matters specifically for GDPR
	// erasure (§15.2): once DeleteByTenant below commits, the tenant's
	// principal row is gone, so the §8.6 orphan-material reconciler (which
	// enumerates the LIVE principal registry to find reclaimable paths)
	// would never find this tenant's material again — a delete-DB-first
	// ordering could leak it forever. Delete is a no-op on an
	// already-missing path, so retrying the whole message after a partial
	// failure here is safe.
	for _, path := range paths {
		if err := c.secrets.Delete(ctx, path); err != nil {
			return err
		}
	}

	return c.tx.RunInTx(ctx, func(ctx context.Context) error {
		if err := c.principals.DeleteByTenant(ctx, payload.TenantID); err != nil {
			return err
		}
		if err := c.processed.MarkProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, env.ID); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		now := time.Now().UTC().Format(time.RFC3339)
		for _, p := range principals {
			if err := pub.Enqueue(ctx, &domain.Event{
				Type: domain.EventServiceAccountRevoked, TenantID: payload.TenantID, Actor: domain.SystemPrincipalID,
				Data: domain.ServiceAccountRevokedPayload{
					TenantID: payload.TenantID, PrincipalID: p.ID, RevokedAt: now,
				},
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *OffboardingConsumer) debug(ctx context.Context, msg, eventID string) {
	if c.log == nil {
		return
	}
	c.log.Debug(msg, withTraceID(ctx, map[string]interface{}{"event_id": eventID}))
}

func (c *OffboardingConsumer) error(ctx context.Context, msg string, fields map[string]interface{}) {
	if c.log == nil {
		return
	}
	c.log.Error(msg, withTraceID(ctx, fields))
}

func withTraceID(ctx context.Context, fields map[string]interface{}) map[string]interface{} {
	if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		if fields == nil {
			fields = map[string]interface{}{}
		}
		fields["trace_id"] = span.SpanContext().TraceID().String()
	}
	return fields
}
