package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/inbound/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// DLQReason message-attribute values, one per permanent-reject class.
const (
	// schemaViolationReason: consumed-schema rejects (inbound_schema.go).
	schemaViolationReason = "schema_violation"
	// invalidEnvelopeIDReason: a TenantMembershipsPurged with a missing or
	// non-UUID envelope id (consumeradapter.ErrInvalidEnvelopeID) — it can
	// never be deduplicated, and acking it would skip a GDPR erasure.
	invalidEnvelopeIDReason = "invalid_envelope_id"
)

// dlqReason reports whether err is a permanent reject that belongs in the
// DLQ now rather than after maxReceiveCount retries, and under which reason.
func dlqReason(err error) (string, bool) {
	switch {
	case errors.Is(err, errSchemaViolation):
		return schemaViolationReason, true
	case errors.Is(err, consumeradapter.ErrInvalidEnvelopeID):
		return invalidEnvelopeIDReason, true
	}
	return "", false
}

// withTraceID adds the active span's trace_id to log fields, matching the
// consumer adapter's own log lines so a reject can be joined to its trace.
func withTraceID(ctx context.Context, fields map[string]any) map[string]any {
	if traceID := gincommon.SpanTraceID(ctx); traceID != "" {
		fields["trace_id"] = traceID
	}
	return fields
}

// dlqSQSClient is the subset of *sqs.Client the DLQ router needs:
// GetQueueAttributes on tenant-lifecycle-tokensvc-q (ConsumeOffboardingQueue)
// and SendMessage on its DLQ (OffboardingDLQPermanentRejects), both granted
// in deploy/iam/policy-consumer.json.
type dlqSQSClient interface {
	GetQueueAttributes(ctx context.Context, params *sqs.GetQueueAttributesInput, optFns ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	SendMessage(ctx context.Context, params *sqs.SendMessageInput, optFns ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// withDLQRouting wraps h with routeRejectsToDLQ, resolving the DLQ from
// queueURL's own RedrivePolicy so it can never drift from the redrive
// target infra configured. If resolution fails the handler is returned
// unwrapped: permanent rejects then still reach the DLQ, just via SQS
// redrive after maxReceiveCount deliveries instead of immediately.
func withDLQRouting(ctx context.Context, client dlqSQSClient, queueURL string, h events.Handler, log port.Logger) events.Handler {
	resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dlqURL, err := resolveDLQURL(resolveCtx, client, queueURL)
	if err != nil {
		log.Warn("DLQ routing disabled — permanent rejects fall back to SQS redrive after maxReceiveCount",
			map[string]any{"queue_url": queueURL, "error": err.Error()})
		return h
	}
	log.Info("DLQ routing wired", map[string]any{"queue_url": queueURL, "dlq_url": dlqURL})
	return routeRejectsToDLQ(h, client, dlqURL, log)
}

// routeRejectsToDLQ sends an envelope whose handler returned a permanent
// reject (see dlqReason) straight to dlqURL and acks the source message.
// platform-events has no "permanent failure" signal — any handler error
// just leaves the message visible — so without this a schema violation
// would burn the full retry budget on a payload that can never pass (or
// an envelope id that can never dedup). If the
// DLQ send fails the original error is returned, so the message
// falls back to normal retry + redrive.
func routeRejectsToDLQ(h events.Handler, client dlqSQSClient, dlqURL string, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := h(ctx, env)
		reason, ok := dlqReason(err)
		if !ok {
			return err
		}
		// The consumer already Glue-decoded Payload; clear dataschema so a
		// later DLQ redrive back to the source queue isn't decoded twice.
		env.SchemaID = ""
		body, mErr := json.Marshal(env)
		if mErr != nil {
			return errors.Join(err, fmt.Errorf("marshal envelope for DLQ: %w", mErr))
		}
		_, sErr := client.SendMessage(ctx, &sqs.SendMessageInput{
			QueueUrl:    aws.String(dlqURL),
			MessageBody: aws.String(string(body)),
			MessageAttributes: map[string]sqstypes.MessageAttributeValue{
				"EventType": {DataType: aws.String("String"), StringValue: aws.String(env.Type)},
				"DLQReason": {DataType: aws.String("String"), StringValue: aws.String(reason)},
			},
		})
		if sErr != nil {
			if log != nil {
				log.Warn("DLQ send failed — falling back to SQS redrive",
					withTraceID(ctx, map[string]any{"event_id": env.ID, "event_type": env.Type, "reason": reason, "dlq_url": dlqURL, "error": sErr.Error()}))
			}
			return err
		}
		metrics.IncConsumerDLQReject(reason)
		logDLQReject(ctx, log, env, reason)
		return nil
	}
}

// logDLQReject records a successful straight-to-DLQ send. The message is
// acked from the source queue at this point, so this line and
// iam_token_service_consumer_dlq_rejects_total are the only in-service
// trace that it left the normal path. An invalid_envelope_id reject is a
// TenantMembershipsPurged whose credential erasure has NOT run and will
// not until someone redrives it, so it logs at Error; a schema violation
// is a producer-contract problem and logs at Warn.
func logDLQReject(ctx context.Context, log port.Logger, env events.Envelope[json.RawMessage], reason string) {
	if log == nil {
		return
	}
	fields := withTraceID(ctx, map[string]any{"reason": reason, "event_type": env.Type, "event_id": env.ID})
	if reason == invalidEnvelopeIDReason {
		log.Error("permanent reject sent to DLQ — tenant credential erasure is pending until it is redriven", fields)
		return
	}
	log.Warn("permanent reject sent to DLQ", fields)
}

// resolveDLQURL returns the URL of queueURL's RedrivePolicy
// deadLetterTargetArn.
func resolveDLQURL(ctx context.Context, client dlqSQSClient, queueURL string) (string, error) {
	out, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameRedrivePolicy},
	})
	if err != nil {
		return "", fmt.Errorf("get RedrivePolicy: %w", err)
	}
	policy := out.Attributes[string(sqstypes.QueueAttributeNameRedrivePolicy)]
	if policy == "" {
		return "", errors.New("queue has no RedrivePolicy")
	}
	var rp struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
	}
	if err := json.Unmarshal([]byte(policy), &rp); err != nil {
		return "", fmt.Errorf("parse RedrivePolicy: %w", err)
	}
	return siblingQueueURL(queueURL, rp.DeadLetterTargetArn)
}

// siblingQueueURL builds the URL of the queue named by arn
// (arn:aws:sqs:<region>:<account>:<name>) by swapping the account and name
// path segments of queueURL. SQS requires a DLQ to share its source queue's
// account and region, so the endpoint (real AWS or floci) carries over.
func siblingQueueURL(queueURL, arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "sqs" || parts[4] == "" || parts[5] == "" {
		return "", fmt.Errorf("deadLetterTargetArn %q is not an SQS queue ARN", arn)
	}
	u, err := url.Parse(queueURL)
	if err != nil {
		return "", fmt.Errorf("parse queue URL: %w", err)
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 2 {
		return "", fmt.Errorf("queue URL %q has no /<account>/<name> path", queueURL)
	}
	segs[len(segs)-2], segs[len(segs)-1] = parts[4], parts[5]
	u.Path = "/" + strings.Join(segs, "/")
	return u.String(), nil
}
