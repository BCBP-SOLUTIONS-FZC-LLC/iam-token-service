package main

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/glue"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/outbox"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// buildGlueCodec returns a GlueCodec whose schema version UUIDs are
// resolved once, by definition, from registryName — fixed for the life of
// the process, so no refresher — or a NoopCodec (plain JSON) when
// registryName is empty (dev/test without a Glue registry configured).
func buildGlueCodec(ctx context.Context, glueClient *glue.Client, registryName string, schemaNames []string) (events.Codec, error) {
	if registryName == "" {
		return events.NoopCodec{}, nil
	}
	return eventbusadapter.NewGlueCodec(ctx, glueClient, registryName, schemaNames)
}

// loadSNSEnv reads SNS publisher config through platform-events
// (SNS_TOPIC_ARN / AWS_REGION / AWS_ENDPOINT_URL), matching iam-user-profile.
// SNS_TOPIC_SERVICEACCOUNT_ARN is accepted as an alias so existing Helm
// values keep working until they also set the library's canonical name.
func loadSNSEnv() eventcfg.SNSConfigEnv {
	env := eventcfg.LoadSNS()
	if env.TopicARN == "" {
		env.TopicARN = os.Getenv("SNS_TOPIC_SERVICEACCOUNT_ARN")
	}
	return env
}

// buildSNSPublisher constructs the SNS publisher from platform-events
// config.LoadSNS. An empty TopicARN yields a noop publisher (dev/test —
// events queue in the outbox but are never actually published).
func buildSNSPublisher(codec events.Codec, log port.Logger) (events.Publisher, error) {
	snsEnv := loadSNSEnv()
	if snsEnv.TopicARN == "" {
		return noopPublisher{}, nil
	}
	return events.NewSNSPublisher(eventcfg.SNSConfigFromEnv(snsEnv, log), events.WithCodec(codec))
}

// loadOutboxEnv reads outbox runner tunables through platform-events
// config.LoadOutbox (OUTBOX_*), matching iam-user-profile. When a var is
// unset, the library default is replaced with this service's deployed
// cadence (500 ms poll, 4-way publish, 2 s jitter, 10 m claim lease) so
// existing Helm charts that do not yet export OUTBOX_* keep LLD §7.4
// behaviour rather than jumping to the library's 5 s poll.
func loadOutboxEnv() eventcfg.OutboxConfigEnv {
	env := eventcfg.LoadOutbox()
	if os.Getenv("OUTBOX_POLL_INTERVAL") == "" {
		env.PollInterval = 500 * time.Millisecond
	}
	if os.Getenv("OUTBOX_PUBLISH_CONCURRENCY") == "" {
		env.PublishConcurrency = 4
	}
	if os.Getenv("OUTBOX_STARTUP_JITTER") == "" {
		env.StartupJitter = 2 * time.Second
	}
	if os.Getenv("OUTBOX_CLAIM_LEASE_DURATION") == "" {
		env.ClaimLeaseDuration = 10 * time.Minute
	}
	return env
}

// newOutboxRunner builds the platform-events outbox.Runner from LoadOutbox
// + RunnerConfigFromEnv, the same wiring as iam-user-profile.
func newOutboxRunner(pool *pgcommon.Pool, publisher events.Publisher, log port.Logger) (*outbox.Runner, error) {
	outboxEnv := loadOutboxEnv()
	eventcfg.LogWarningsTo(log, outboxEnv.Warnings)
	return outbox.NewRunner(eventcfg.RunnerConfigFromEnv(outboxEnv, pool, publisher, log))
}

type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, events.Envelope[json.RawMessage]) error { return nil }
func (noopPublisher) PublishBatch(context.Context, []events.Envelope[json.RawMessage]) error {
	return nil
}
