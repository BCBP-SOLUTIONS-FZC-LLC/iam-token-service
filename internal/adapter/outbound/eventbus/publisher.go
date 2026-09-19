package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/outbox"
)

// Publisher implements port.EventPublisher. It JSON-marshals the event
// payload, validates it via the configured Codec (schema validation only —
// no wire encoding), wraps it as plain JSON in an events.Envelope, and
// inserts it into outbox_events on the transaction TxRunner attached to
// ctx — atomic with the business write (EVT-1).
//
// Glue/wire encoding is NOT performed here. It is deferred to the SNS
// publisher (outbox runner publish path) via events.WithCodec so the
// outbox stores human-readable plain JSON for observability and replay
// (§7.3.1).
type Publisher struct {
	source string
	codec  events.Codec
	log    port.Logger
}

// New builds a Publisher that stamps source on every enqueued envelope and
// validates payloads via codec. A nil codec becomes events.NoopCodec.
func New(source string, codec events.Codec) *Publisher {
	if codec == nil {
		codec = events.NoopCodec{}
	}
	return &Publisher{source: source, codec: codec}
}

// WithLogger attaches a logger for enqueue diagnostics and returns the
// publisher for chaining.
func (p *Publisher) WithLogger(log port.Logger) *Publisher {
	p.log = log
	return p
}

var _ port.EventPublisher = (*Publisher)(nil)

// Enqueue validates and writes one plain-JSON envelope into outbox_events
// on the transaction TxRunner stored in ctx. Glue encoding is deferred to
// the outbox runner's SNS publisher (WithCodec).
func (p *Publisher) Enqueue(ctx context.Context, event *domain.Event) error {
	raw, err := json.Marshal(event.Data)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	if _, _, err := p.codec.Encode(ctx, event.Type, raw); err != nil {
		return fmt.Errorf("validate event %s: %w", event.Type, err)
	}
	opts := []events.EnvelopeOpt{
		events.WithTenantID(event.TenantID.String()),
		events.WithActor(event.Actor.String()),
		events.WithSchemaVersion("1"),
	}
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		opts = append(opts,
			events.WithTraceID(spanCtx.TraceID().String()),
			events.WithCorrelationID(spanCtx.SpanID().String()),
		)
	}
	env := events.NewEnvelope(event.Type, p.source, json.RawMessage(raw), opts...)
	tx, ok := port.TxFromContext(ctx)
	if !ok {
		return errors.New("event enqueue requires an open RunInTx transaction")
	}
	if err := outbox.Enqueue(ctx, tx, env); err != nil {
		p.warn(ctx, "outbox.Enqueue failed", event.Type, err)
		return err
	}
	p.debug(ctx, "outbox.Enqueue", event.Type, env.ID)
	return nil
}

func (p *Publisher) warn(ctx context.Context, msg, eventType string, err error) {
	if p.log == nil {
		return
	}
	fields := map[string]interface{}{"type": eventType, "error": err.Error()}
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		fields["trace_id"] = spanCtx.TraceID().String()
	}
	p.log.Warn(msg, fields)
}

func (p *Publisher) debug(ctx context.Context, msg, eventType, eventID string) {
	if p.log == nil {
		return
	}
	fields := map[string]interface{}{"type": eventType, "event_id": eventID}
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		fields["trace_id"] = spanCtx.TraceID().String()
	}
	p.log.Debug(msg, fields)
}
