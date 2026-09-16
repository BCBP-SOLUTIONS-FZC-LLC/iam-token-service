// Package eventbus implements the outbound port.EventPublisher (writes to
// the transactional outbox, EVT-1) and the wire-format codec used at
// SNS-publish time (AWS Glue Schema Registry, §7.3.1).
package eventbus

import "context"

// Codec is the enqueue-time schema-validation hook (payload shape only —
// no wire encoding happens here; that is deferred to the outbox runner's
// SNS publisher via events.WithCodec, so the outbox always stores
// human-readable plain JSON, §7.3.1).
type Codec interface {
	Encode(ctx context.Context, eventType string, payload []byte) (encoded []byte, schemaVersionID string, err error)
}

// NoopCodec passes payload through unchanged — the base every enqueue-time
// codec wraps.
type NoopCodec struct{}

// Encode returns payload unchanged, performing no schema validation or wire
// encoding.
func (NoopCodec) Encode(_ context.Context, _ string, payload []byte) (encoded []byte, schemaVersionID string, err error) {
	return payload, "", nil
}
