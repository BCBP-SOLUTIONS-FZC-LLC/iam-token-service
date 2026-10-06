// Package consumer implements this service's one inbound SQS subscription:
// TenantMembershipsPurged on tenant-lifecycle-tokensvc-q, driving the
// tenant-offboarding cascade (§7.1, §8.4).
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// tenantMembershipsPurgedEventType is the wire-format `type` field on a
// TenantMembershipsPurged envelope, published by iam-org-membership onto
// the tenant-lifecycle queue this service consumes from.
const tenantMembershipsPurgedEventType = "TenantMembershipsPurged"

// OffboardingConsumer implements events.Handler for TenantMembershipsPurged
// (§8.4): hard-deletes the tenant's service_account_* rows and every
// credential's OpenBao material, then emits ServiceAccountRevoked per
// principal found. Exactly-once via platform-events' inbox over
// processed_events (consumer='tenant_offboarding'), keyed on the envelope id
// (§9.2): the claim, the cascade's Postgres writes and its outbox events
// commit in one transaction.
// Unknown types are silently acked (iam-user-profile / iam-org-membership
// ackUnknown) so a producer schema addition does not DLQ-storm.
type OffboardingConsumer struct {
	principals  port.PrincipalRepository
	credentials port.CredentialRepository
	secrets     port.SecretStore
	inbox       port.Inbox
	log         port.Logger
}

// NewOffboardingConsumer wires OffboardingConsumer's dependencies.
func NewOffboardingConsumer(
	principals port.PrincipalRepository,
	credentials port.CredentialRepository,
	secrets port.SecretStore,
	inbox port.Inbox,
	log port.Logger,
) *OffboardingConsumer {
	return &OffboardingConsumer{
		principals: principals, credentials: credentials, secrets: secrets,
		inbox: inbox, log: log,
	}
}

// ErrInvalidEnvelopeID marks a TenantMembershipsPurged whose envelope id
// is missing or not a UUID. It is permanent — redelivery carries the same
// id — so the DLQ router treats it as a reject, not a retry.
var ErrInvalidEnvelopeID = errors.New("TenantMembershipsPurged envelope id is missing or not a UUID")

// validEnvelopeID reports whether id can key processed_events: the inbox
// dedups on the envelope id, which the platform envelope contract makes a
// UUID.
func validEnvelopeID(id string) bool {
	if id == "" {
		return false
	}
	_, err := uuid.Parse(id)
	return err == nil
}

// tenantMembershipsPurgedPayload extracts only the field this consumer
// needs. The canonical schema for TenantMembershipsPurged is owned by its
// producing service, not this repo (§7.1) — additional fields are ignored,
// not validated.
type tenantMembershipsPurgedPayload struct {
	TenantID uuid.UUID `json:"tenant_id"`
}

// Handle implements events.Handler. The span is started on gincommon's
// process-global TracerProvider (cmd/consumer's InitTracingWithConfig) so
// offboarding work joins the same OTLP pipeline as HTTP/db spans.
func (c *OffboardingConsumer) Handle(ctx context.Context, env events.Envelope[json.RawMessage]) error {
	ctx, span := gincommon.NewTracer("iam-token-service").Start(ctx, "consumer.offboarding")
	defer span.End()

	if env.Type != tenantMembershipsPurgedEventType {
		// Forward-compat: a producer schema addition must never DLQ-storm
		// this queue, so an unknown type is acked even when its id is bad
		// (without an id it cannot be recorded in processed_events either).
		if !validEnvelopeID(env.ID) {
			c.error(ctx, "unknown event type with invalid or missing envelope id — acknowledging to prevent redelivery loop", map[string]interface{}{
				"event_type": env.Type, "event_id": env.ID,
			})
			return nil
		}
		return ackUnknown(ctx, c.inbox, c.log, port.ProcessedEventsConsumerTenantOffboarding, env)
	}

	// A TenantMembershipsPurged without a dedup key cannot run exactly-once,
	// but acking it would silently skip a GDPR erasure (§15.2) forever. It
	// is a permanent reject instead: cmd/consumer's DLQ router sends it
	// straight to the DLQ (DLQReason=invalid_envelope_id) for an operator to
	// fix and redrive, or normal SQS redrive gets it there if the router is
	// unavailable.
	if !validEnvelopeID(env.ID) {
		c.error(ctx, "TenantMembershipsPurged has invalid or missing envelope id — rejecting to DLQ; tenant erasure has NOT run", map[string]interface{}{
			"event_type": env.Type, "event_id": env.ID,
		})
		return fmt.Errorf("offboarding consumer: %w: %q", ErrInvalidEnvelopeID, env.ID)
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
	// reconciler jobs, RLS-7). The inbox transaction runs on the same pool,
	// so the GUC is bound with SET LOCAL inside it (RLS-6).
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = domain.SystemPrincipalID.String()
	g.TenantID = payload.TenantID.String()
	ctx = pgcommon.WithGUCSet(ctx, g)

	duplicate, err := processOnce(ctx, c.inbox, port.ProcessedEventsConsumerTenantOffboarding, env, func(txCtx context.Context) error {
		return c.cascade(txCtx, payload.TenantID)
	})
	if err != nil {
		return err
	}
	if duplicate {
		c.debug(ctx, "offboarding event already processed, skipping", env.ID)
	}
	return nil
}

// cascade runs inside the inbox transaction, after the processed_events
// claim: the claim's row lock serialises concurrent deliveries of the same
// event, so the cascade runs once.
func (c *OffboardingConsumer) cascade(ctx context.Context, tenantID uuid.UUID) error {
	principals, err := c.principals.LockByTenant(ctx, tenantID)
	if err != nil {
		return err
	}

	// Collect every credential's OpenBao path across every principal
	// BEFORE any delete.
	var paths []string
	for _, p := range principals {
		creds, err := c.credentials.ListByPrincipal(ctx, tenantID, p.ID)
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
	// ordering could leak it forever. If anything fails after a delete, the
	// transaction (claim included) rolls back and the redelivery repeats the
	// cascade; Delete is a no-op on an already-missing path, so that is safe.
	for _, path := range paths {
		if err := c.secrets.Delete(ctx, path); err != nil {
			return err
		}
	}
	// Then erase whatever else lives under the tenant's subtree: material a
	// failed or racing TS-1 wrote but never committed has no row, so the
	// path list above cannot see it, and once the principal row is gone the
	// §8.6 reconciler can never find it either (§15.2 erasure). The principal
	// rows are locked (LockByTenant … FOR UPDATE), so no new issue/rotate
	// for this tenant can write more material until this transaction ends.
	if err := c.deleteSubtree(ctx, domain.OpenBaoTenantPrefix(tenantID), 0); err != nil {
		return err
	}

	if err := c.principals.DeleteByTenant(ctx, tenantID); err != nil {
		return err
	}
	pub, ok := port.EventPublisherFromContext(ctx)
	if !ok {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, p := range principals {
		if err := pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventServiceAccountRevoked, TenantID: tenantID, Actor: domain.SystemPrincipalID,
			Data: domain.ServiceAccountRevokedPayload{
				TenantID: tenantID, PrincipalID: p.ID, RevokedAt: now,
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

// maxSubtreeDepth bounds the recursive walk: the frozen layout is
// <tenant>/<keycloak_client_id>/v<n> (§6.3), so anything deeper is not ours.
const maxSubtreeDepth = 4

// deleteSubtree deletes every leaf under prefix. SecretStore.List returns
// one level, with sub-folders suffixed "/".
func (c *OffboardingConsumer) deleteSubtree(ctx context.Context, prefix string, depth int) error {
	if depth > maxSubtreeDepth {
		return nil
	}
	keys, err := c.secrets.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		child := prefix + "/" + strings.TrimSuffix(key, "/")
		if strings.HasSuffix(key, "/") {
			if err := c.deleteSubtree(ctx, child, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := c.secrets.Delete(ctx, child); err != nil {
			return err
		}
	}
	return nil
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
	if traceID := gincommon.SpanTraceID(ctx); traceID != "" {
		if fields == nil {
			fields = map[string]interface{}{}
		}
		fields["trace_id"] = traceID
	}
	return fields
}
