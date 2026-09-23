package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

const (
	flociQueueURL = "http://floci:4566/000000000000/tenant-lifecycle-tokensvc-q"
	flociDLQURL   = "http://floci:4566/000000000000/tenant-lifecycle-tokensvc-q-dlq"
	flociDLQARN   = "arn:aws:sqs:ap-south-1:000000000000:tenant-lifecycle-tokensvc-q-dlq"
)

// fakeDLQClient is a dlqSQSClient test double.
type fakeDLQClient struct {
	attrs   map[string]string
	getErr  error
	sendErr error
	sent    []*sqs.SendMessageInput
}

func (f *fakeDLQClient) GetQueueAttributes(_ context.Context, _ *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &sqs.GetQueueAttributesOutput{Attributes: f.attrs}, nil
}

func (f *fakeDLQClient) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.sent = append(f.sent, in)
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &sqs.SendMessageOutput{}, nil
}

func redriveAttrs(arn string) map[string]string {
	return map[string]string{"RedrivePolicy": `{"deadLetterTargetArn":"` + arn + `","maxReceiveCount":"5"}`}
}

func handlerReturning(err error) events.Handler {
	return func(context.Context, events.Envelope[json.RawMessage]) error { return err }
}

func testEnvelope() events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{
		ID: "evt-1", Type: "TenantMembershipsPurged", Source: "iam-org-membership",
		TenantID: uuid.NewString(), SchemaID: uuid.NewString(),
		Timestamp: time.Now().Add(time.Hour).UTC(), Payload: json.RawMessage(`{"tenant_id":"6f1c2c1e-0000-4000-8000-000000000001"}`),
	}
}

// ── routeRejectsToDLQ ──────────────────────────────────────────────────

func TestRouteRejects_SchemaViolation_SentToDLQAndAcked(t *testing.T) {
	client := &fakeDLQClient{}
	h := routeRejectsToDLQ(handlerReturning(errSchemaViolation), client, flociDLQURL, nil)

	env := testEnvelope()
	require.NoError(t, h(context.Background(), env), "a DLQ'd reject must ack the source message")

	require.Len(t, client.sent, 1)
	in := client.sent[0]
	assert.Equal(t, flociDLQURL, aws.ToString(in.QueueUrl))
	assert.Equal(t, env.Type, aws.ToString(in.MessageAttributes["EventType"].StringValue))
	assert.Equal(t, schemaViolationReason, aws.ToString(in.MessageAttributes["DLQReason"].StringValue))

	var got events.Envelope[json.RawMessage]
	require.NoError(t, json.Unmarshal([]byte(aws.ToString(in.MessageBody)), &got))
	assert.Equal(t, env.ID, got.ID)
	assert.JSONEq(t, string(env.Payload), string(got.Payload), "DLQ body carries the already-decoded plain payload")
	assert.Empty(t, got.SchemaID, "dataschema must be cleared so a redrive isn't Glue-decoded twice")
}

func TestRouteRejects_WrappedViolation_StillRouted(t *testing.T) {
	client := &fakeDLQClient{}
	wrapped := fmt.Errorf("%w: detail", errSchemaViolation)
	h := routeRejectsToDLQ(handlerReturning(wrapped), client, flociDLQURL, nil)
	require.NoError(t, h(context.Background(), testEnvelope()))
	assert.Len(t, client.sent, 1)
}

func TestRouteRejects_SendFails_ReturnsOriginalErrorForRedrive(t *testing.T) {
	client := &fakeDLQClient{sendErr: errors.New("throttled")}
	log := &fakeLogger{}
	h := routeRejectsToDLQ(handlerReturning(errSchemaViolation), client, flociDLQURL, log)

	err := h(context.Background(), testEnvelope())
	assert.ErrorIs(t, err, errSchemaViolation, "send failure must leave the message visible")
	assert.Equal(t, int32(1), log.warnCalls)
}

func TestRouteRejects_SendFails_NilLogger_NoPanic(t *testing.T) {
	client := &fakeDLQClient{sendErr: errors.New("throttled")}
	h := routeRejectsToDLQ(handlerReturning(errSchemaViolation), client, flociDLQURL, nil)
	assert.ErrorIs(t, h(context.Background(), testEnvelope()), errSchemaViolation)
}

func TestRouteRejects_OtherOutcomes_PassThroughUntouched(t *testing.T) {
	transient := errors.New("db down")
	for name, want := range map[string]error{"success": nil, "transient error": transient} {
		t.Run(name, func(t *testing.T) {
			client := &fakeDLQClient{}
			h := routeRejectsToDLQ(handlerReturning(want), client, flociDLQURL, nil)
			assert.Equal(t, want, h(context.Background(), testEnvelope()))
			assert.Empty(t, client.sent, "only ErrPoisonPill is sent to the DLQ")
		})
	}
}

// ── resolveDLQURL / siblingQueueURL ────────────────────────────────────────

func TestResolveDLQURL_FromRedrivePolicy(t *testing.T) {
	got, err := resolveDLQURL(context.Background(), &fakeDLQClient{attrs: redriveAttrs(flociDLQARN)}, flociQueueURL)
	require.NoError(t, err)
	assert.Equal(t, flociDLQURL, got)
}

func TestResolveDLQURL_Errors(t *testing.T) {
	cases := map[string]*fakeDLQClient{
		"get error":        {getErr: errors.New("access denied")},
		"no redrive":       {attrs: map[string]string{}},
		"bad policy json":  {attrs: map[string]string{"RedrivePolicy": "{"}},
		"non-sqs dlq arn":  {attrs: redriveAttrs("arn:aws:sns:ap-south-1:000000000000:topic")},
		"empty target arn": {attrs: map[string]string{"RedrivePolicy": `{}`}},
	}
	for name, client := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := resolveDLQURL(context.Background(), client, flociQueueURL)
			assert.Error(t, err)
		})
	}
}

func TestSiblingQueueURL_RealAWSEndpoint(t *testing.T) {
	got, err := siblingQueueURL(
		"https://sqs.ap-south-1.amazonaws.com/123456789012/tenant-lifecycle-tokensvc-q",
		"arn:aws:sqs:ap-south-1:123456789012:tenant-lifecycle-tokensvc-q-dlq",
	)
	require.NoError(t, err)
	assert.Equal(t, "https://sqs.ap-south-1.amazonaws.com/123456789012/tenant-lifecycle-tokensvc-q-dlq", got)
}

func TestSiblingQueueURL_Errors(t *testing.T) {
	_, err := siblingQueueURL("http://floci:4566/", flociDLQARN)
	assert.Error(t, err, "URL without /<account>/<name> path")
	_, err = siblingQueueURL("://bad", flociDLQARN)
	assert.Error(t, err, "unparseable URL")
}

// ── withDLQRouting ──────────────────────────────────────────────────────

func TestWithPoisonPillDLQ_Resolved_WrapsHandler(t *testing.T) {
	client := &fakeDLQClient{attrs: redriveAttrs(flociDLQARN)}
	h := withDLQRouting(context.Background(), client, flociQueueURL, handlerReturning(errSchemaViolation), &fakeLogger{})
	require.NoError(t, h(context.Background(), testEnvelope()))
	assert.Len(t, client.sent, 1)
}

func TestWithPoisonPillDLQ_Unresolvable_FallsBackToRedrive(t *testing.T) {
	client := &fakeDLQClient{getErr: errors.New("access denied")}
	log := &fakeLogger{}
	h := withDLQRouting(context.Background(), client, flociQueueURL, handlerReturning(errSchemaViolation), log)
	assert.ErrorIs(t, h(context.Background(), testEnvelope()), errSchemaViolation)
	assert.Empty(t, client.sent)
	assert.Equal(t, int32(1), log.warnCalls)
}

// ── buildSQSConsumer — consumer-side Glue decode ───────────────────────────

// oneShotSQS delivers one message on the first ReceiveMessage, then idles.
type oneShotSQS struct {
	mu      sync.Mutex
	msg     *sqstypes.Message
	deleted chan struct{}
}

func (s *oneShotSQS) ReceiveMessage(ctx context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	s.mu.Lock()
	msg := s.msg
	s.msg = nil
	s.mu.Unlock()
	if msg != nil {
		return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{*msg}}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(20 * time.Millisecond):
		return &sqs.ReceiveMessageOutput{}, nil
	}
}

func (s *oneShotSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	close(s.deleted)
	return &sqs.DeleteMessageOutput{}, nil
}

func (s *oneShotSQS) ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

// TestBuildSQSConsumer_DecodesGlueEncodedUpstreamPayload proves the
// consumer carries a codec: an upstream envelope with a dataschema and a
// base64 Glue-framed payload reaches the handler as plain JSON and is acked.
// Without WithConsumerCodec platform-events fails decode and never deletes.
func TestBuildSQSConsumer_DecodesGlueEncodedUpstreamPayload(t *testing.T) {
	versionID := uuid.New()
	plain := []byte(`{"tenant_id":"6f1c2c1e-0000-4000-8000-000000000001"}`)
	framed := append([]byte{0x03, 0x00}, versionID[:]...)
	framed = append(framed, plain...)
	data, err := json.Marshal(framed) // []byte marshals as a base64 JSON string (WrapCodecPayload's format)
	require.NoError(t, err)

	body, err := json.Marshal(map[string]any{
		"id": "evt-1", "type": "TenantMembershipsPurged", "source": "iam-org-membership",
		"tenant_id": uuid.NewString(), "dataschema": versionID.String(),
		"time": time.Now().UTC(), "data": json.RawMessage(data),
	})
	require.NoError(t, err)

	client := &oneShotSQS{
		msg:     &sqstypes.Message{MessageId: aws.String("m-1"), ReceiptHandle: aws.String("rh-1"), Body: aws.String(string(body))},
		deleted: make(chan struct{}),
	}
	got := make(chan json.RawMessage, 1)
	handler := func(_ context.Context, env events.Envelope[json.RawMessage]) error {
		got <- env.Payload
		return nil
	}

	env := eventcfg.SQSConfigEnv{QueueURL: flociQueueURL, Region: "ap-south-1", Concurrency: 1, MaxMessages: 1, WaitSeconds: 1, VisibilityTimeout: 30 * time.Second}
	cons, err := buildSQSConsumer(env, client, handler, nil)
	require.NoError(t, err)

	go func() { _ = cons.Start(t.Context()) }()
	defer func() { _ = cons.Stop() }()

	select {
	case payload := <-got:
		assert.JSONEq(t, string(plain), string(payload))
	case <-time.After(5 * time.Second):
		t.Fatal("handler never received the message — Glue decode likely failed")
	}
	select {
	case <-client.deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("message was not acked")
	}
}

// fakeLogger is a minimal port.Logger test double counting Warn calls.
type fakeLogger struct {
	mu        sync.Mutex
	warnCalls int32
}

func (f *fakeLogger) Debug(string, map[string]any) {}
func (f *fakeLogger) Info(string, map[string]any)  {}
func (f *fakeLogger) Warn(string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warnCalls++
}
func (f *fakeLogger) Error(string, map[string]any) {}
