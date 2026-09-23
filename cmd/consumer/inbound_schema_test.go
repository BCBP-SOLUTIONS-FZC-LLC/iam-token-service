package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// realValidator is the production ValidatingCodec over the embedded
// schemas/*.json, so these tests exercise the actual consumed contracts.
func realValidator(t *testing.T) *eventbusadapter.ValidatingCodec {
	t.Helper()
	v, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	require.NoError(t, err)
	return v
}

// recordingHandler counts calls and returns err.
type recordingHandler struct {
	calls int
	err   error
}

func (r *recordingHandler) handle(context.Context, events.Envelope[json.RawMessage]) error {
	r.calls++
	return r.err
}

func envelopeOf(eventType, payload string) events.Envelope[json.RawMessage] {
	env := testEnvelope()
	env.Type = eventType
	env.Payload = json.RawMessage(payload)
	return env
}

const purgedTenant = "6f1c2c1e-0000-4000-8000-000000000001"

// iam-org-membership's real TenantMembershipsPurged payload (its producer
// schema also carries an optional actor_id).
func TestValidateConsumed_RealProducerPayload_ReachesHandler(t *testing.T) {
	inner := &recordingHandler{}
	h := validateConsumed(inner.handle, realValidator(t), nil)
	require.NoError(t, h(context.Background(), envelopeOf("TenantMembershipsPurged", `{"tenant_id":"`+purgedTenant+`","actor_id":"`+purgedTenant+`","added_later":true}`)))
	assert.Equal(t, 1, inner.calls, "OPEN schema — extra fields accepted")
}

func TestValidateConsumed_Violations_RejectedBeforeHandler(t *testing.T) {
	cases := map[string]string{
		"missing tenant_id": `{"actor_id":"` + purgedTenant + `"}`,
		"wrong type":        `{"tenant_id":42}`,
		"not JSON":          `not-json`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			inner := &recordingHandler{}
			log := &fakeLogger{}
			h := validateConsumed(inner.handle, realValidator(t), log)
			err := h(context.Background(), envelopeOf("TenantMembershipsPurged", payload))
			assert.ErrorIs(t, err, errSchemaViolation)
			assert.Zero(t, inner.calls, "a violating payload must never reach the offboarding cascade")
			assert.Equal(t, int32(1), log.warnCalls)
			reason, ok := dlqReason(err)
			assert.True(t, ok)
			assert.Equal(t, schemaViolationReason, reason)
		})
	}
}

func TestValidateConsumed_UnknownType_PassesThroughForAckUnknown(t *testing.T) {
	inner := &recordingHandler{}
	h := validateConsumed(inner.handle, realValidator(t), nil)
	require.NoError(t, h(context.Background(), envelopeOf("SomeFutureEvent", `{"anything":true}`)))
	assert.Equal(t, 1, inner.calls)
}

func TestValidateConsumed_HandlerErrorPropagatesUnchanged(t *testing.T) {
	transient := errors.New("openbao down")
	inner := &recordingHandler{err: transient}
	h := validateConsumed(inner.handle, realValidator(t), nil)
	err := h(context.Background(), envelopeOf("TenantMembershipsPurged", `{"tenant_id":"`+purgedTenant+`"}`))
	assert.Equal(t, transient, err)
	_, ok := dlqReason(err)
	assert.False(t, ok, "transient handler errors must keep normal retry semantics")
}

// TestPipeline_SchemaViolation_EndsUpInDLQ exercises the composed pipeline
// main.go builds: violation → DLQ with DLQReason=schema_violation → ack,
// and the offboarding cascade never runs.
func TestPipeline_SchemaViolation_EndsUpInDLQ(t *testing.T) {
	client := &fakeDLQClient{attrs: redriveAttrs(flociDLQARN)}
	inner := &recordingHandler{}
	h := withDLQRouting(context.Background(), client, flociQueueURL, validateConsumed(inner.handle, realValidator(t), &fakeLogger{}), &fakeLogger{})

	require.NoError(t, h(context.Background(), envelopeOf("TenantMembershipsPurged", `{}`)))
	assert.Zero(t, inner.calls)
	require.Len(t, client.sent, 1)
	assert.Equal(t, schemaViolationReason, aws.ToString(client.sent[0].MessageAttributes["DLQReason"].StringValue))
}

func TestPipeline_ValidEvent_NoDLQ(t *testing.T) {
	client := &fakeDLQClient{attrs: redriveAttrs(flociDLQARN)}
	inner := &recordingHandler{}
	h := withDLQRouting(context.Background(), client, flociQueueURL, validateConsumed(inner.handle, realValidator(t), &fakeLogger{}), &fakeLogger{})

	require.NoError(t, h(context.Background(), envelopeOf("TenantMembershipsPurged", `{"tenant_id":"`+purgedTenant+`"}`)))
	assert.Equal(t, 1, inner.calls)
	assert.Empty(t, client.sent)
}
