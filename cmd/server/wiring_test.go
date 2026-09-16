package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/glue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
)

// newUnreachableGlueClient returns a *glue.Client pointed at a closed local
// port with retries disabled, so a call against it fails fast (connection
// refused) instead of retrying AWS SDK's default 3 attempts with backoff.
func newUnreachableGlueClient(t *testing.T) *glue.Client {
	t.Helper()
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: awscreds.NewStaticCredentialsProvider("fake", "fake", ""),
		Retryer: func() aws.Retryer {
			return retry.AddWithMaxAttempts(retry.NewStandard(), 1)
		},
	}
	return glue.NewFromConfig(cfg, func(o *glue.Options) {
		o.BaseEndpoint = aws.String("http://127.0.0.1:1") // reserved, always closed
	})
}

func TestBuildGlueCodec_EmptyRegistryNameReturnsNoop(t *testing.T) {
	codec, err := buildGlueCodec(t.Context(), nil, "", nil, nil)
	require.NoError(t, err)
	assert.IsType(t, events.NoopCodec{}, codec)
}

func TestBuildGlueCodec_PrefetchFailurePropagates(t *testing.T) {
	client := newUnreachableGlueClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := buildGlueCodec(ctx, client, "iam-serviceaccount-events", []string{"ServiceAccountRegistered"}, nil)
	require.Error(t, err)
}

func TestBuildSNSPublisher_EmptyTopicARNReturnsNoop(t *testing.T) {
	t.Setenv("SNS_TOPIC_ARN", "")
	t.Setenv("SNS_TOPIC_SERVICEACCOUNT_ARN", "")
	pub, err := buildSNSPublisher(events.NoopCodec{}, nil)
	require.NoError(t, err)
	_, ok := pub.(noopPublisher)
	assert.True(t, ok)
}

func TestBuildSNSPublisher_ConstructsRealPublisherWithoutDialing(t *testing.T) {
	// events.NewSNSPublisher / the underlying AWS SNS client constructor
	// does not dial the network at construction time — only Publish does
	// — so this should succeed even with a fake region/ARN.
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("SNS_TOPIC_ARN", "arn:aws:sns:us-east-1:123456789012:iam-serviceaccount-events")
	pub, err := buildSNSPublisher(events.NoopCodec{}, nil)
	require.NoError(t, err)
	assert.NotNil(t, pub)
}

func TestLoadSNSEnv_FallsBackToServiceAccountAlias(t *testing.T) {
	t.Setenv("SNS_TOPIC_ARN", "")
	t.Setenv("SNS_TOPIC_SERVICEACCOUNT_ARN", "arn:aws:sns:us-east-1:1:alias")
	env := loadSNSEnv()
	assert.Equal(t, "arn:aws:sns:us-east-1:1:alias", env.TopicARN)
}

func TestLoadOutboxEnv_AppliesServiceDefaultsWhenUnset(t *testing.T) {
	t.Setenv("OUTBOX_POLL_INTERVAL", "")
	t.Setenv("OUTBOX_PUBLISH_CONCURRENCY", "")
	t.Setenv("OUTBOX_STARTUP_JITTER", "")
	t.Setenv("OUTBOX_CLAIM_LEASE_DURATION", "")
	env := loadOutboxEnv()
	assert.Equal(t, 500*time.Millisecond, env.PollInterval)
	assert.Equal(t, 4, env.PublishConcurrency)
	assert.Equal(t, 2*time.Second, env.StartupJitter)
	assert.Equal(t, 10*time.Minute, env.ClaimLeaseDuration)
}

func TestLoadOutboxEnv_HonoursExplicitEnv(t *testing.T) {
	t.Setenv("OUTBOX_POLL_INTERVAL", "2s")
	t.Setenv("OUTBOX_PUBLISH_CONCURRENCY", "8")
	t.Setenv("OUTBOX_STARTUP_JITTER", "5s")
	t.Setenv("OUTBOX_CLAIM_LEASE_DURATION", "15m")
	env := loadOutboxEnv()
	assert.Equal(t, 2*time.Second, env.PollInterval)
	assert.Equal(t, 8, env.PublishConcurrency)
	assert.Equal(t, 5*time.Second, env.StartupJitter)
	assert.Equal(t, 15*time.Minute, env.ClaimLeaseDuration)
}

func TestNoopPublisher_PublishAndPublishBatch(t *testing.T) {
	var p noopPublisher
	assert.NoError(t, p.Publish(t.Context(), events.Envelope[json.RawMessage]{}))
	assert.NoError(t, p.PublishBatch(t.Context(), nil))
}
