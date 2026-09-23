package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// errSchemaViolation marks a consumed payload that fails its embedded
// schema. It is permanent — the same bytes fail on every redelivery — so
// routeRejectsToDLQ sends it straight to the DLQ.
var errSchemaViolation = errors.New("consumed event payload violates its embedded schema")

// payloadValidator is satisfied by *eventbus.ValidatingCodec — the same
// compiled schemas/*.json that gate produced events at outbox-enqueue time
// (tenant_memberships_purged.json is this service's one consumed schema).
type payloadValidator interface {
	Validate(eventType string, payload []byte) error
}

// validateConsumed checks each inbound envelope's (already Glue-decoded)
// payload against the embedded schema for env.Type before h runs — this
// service's own contract for what it relies on (api/asyncapi.yaml), not the
// producer's Glue schema, so it needs no Glue call and keeps working
// through a Glue outage.
//
// An event type with no embedded schema passes through untouched, so
// OffboardingConsumer's ackUnknown forward-compat path still sees it. A
// violation is logged, counted as
// iam_token_service_consumed_schema_violations_total, and returned as
// errSchemaViolation for the DLQ router.
func validateConsumed(h events.Handler, v payloadValidator, log port.Logger) events.Handler {
	return func(ctx context.Context, env events.Envelope[json.RawMessage]) error {
		err := v.Validate(env.Type, env.Payload)
		if err == nil || errors.Is(err, eventbusadapter.ErrNoSchema) {
			return h(ctx, env)
		}
		if metrics.ConsumedSchemaViolationsTotal != nil {
			metrics.ConsumedSchemaViolationsTotal.WithLabelValues(string(port.ProcessedEventsConsumerTenantOffboarding), env.Type).Inc()
		}
		if log != nil {
			log.Warn("consumed event violates its embedded schema — rejecting to DLQ",
				map[string]any{"event_id": env.ID, "event_type": env.Type, "source": env.Source, "error": err.Error()})
		}
		return fmt.Errorf("%w: %w", errSchemaViolation, err)
	}
}

// newSQSClient builds the one *sqs.Client this process uses — shared by the
// platform-events consumer (NewSQSConsumerWithClient) and the DLQ router —
// from the same region/endpoint platform-events would otherwise use to
// build its own.
func newSQSClient(ctx context.Context, env eventcfg.SQSConfigEnv) (*sqs.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(env.Region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if env.EndpointURL != "" {
			o.BaseEndpoint = &env.EndpointURL
		}
	}), nil
}

// buildSQSConsumer constructs the offboarding consumer from the
// LoadSQS-derived env plus SQSConsumerOptions and the consumer-side
// GlueDecoder. Without a consumer codec, a Glue-encoded
// TenantMembershipsPurged (iam-org-membership publishes through its own
// GlueCodec) fails decode on every delivery and ends up in the DLQ — the
// tenant's service accounts would never be cleaned up.
func buildSQSConsumer(env eventcfg.SQSConfigEnv, client events.SQSClientLike, handler events.Handler, log port.Logger) (events.Consumer, error) {
	return events.NewSQSConsumerWithClient(
		eventcfg.SQSConfigFromEnv(env, log),
		client,
		handler,
		append(eventcfg.SQSConsumerOptions(env), events.WithConsumerCodec(eventbusadapter.GlueDecoder{}))...,
	)
}
