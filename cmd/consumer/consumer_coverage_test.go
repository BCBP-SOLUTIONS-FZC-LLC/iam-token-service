package main

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventcfg "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// An envelope whose (already-decoded) payload is not valid JSON cannot be
// marshalled for the DLQ: the router must not send anything, must not ack,
// and must return the original error joined with the marshal failure so the
// message falls back to normal SQS redrive.
func TestRouteRejectsToDLQ_MarshalFailureFallsBackToRedrive(t *testing.T) {
	client := &fakeDLQClient{}
	h := routeRejectsToDLQ(handlerReturning(errSchemaViolation), client, flociDLQURL, &fakeLogger{})

	err := h(t.Context(), events.Envelope[json.RawMessage]{
		ID:      "evt-1",
		Type:    "TenantMembershipsPurged",
		Payload: json.RawMessage(`{not json`),
	})

	require.Error(t, err)
	require.ErrorIs(t, err, errSchemaViolation, "the original error is preserved")
	assert.Contains(t, err.Error(), "marshal envelope for DLQ")
	assert.Empty(t, client.sent, "nothing is sent when the body cannot be built")
}

func TestNewSQSClient_UsesRegionAndEndpointOverride(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client, err := newSQSClient(t.Context(), eventcfg.SQSConfigEnv{Region: "ap-south-1", EndpointURL: "http://floci:4566"})
	require.NoError(t, err)
	opts := client.Options()
	assert.Equal(t, "ap-south-1", opts.Region)
	require.NotNil(t, opts.BaseEndpoint)
	assert.Equal(t, "http://floci:4566", aws.ToString(opts.BaseEndpoint))
}

func TestNewSQSClient_NoEndpointLeavesAWSDefault(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	client, err := newSQSClient(t.Context(), eventcfg.SQSConfigEnv{Region: "us-east-1"})
	require.NoError(t, err)
	assert.Equal(t, "us-east-1", client.Options().Region)
	assert.Nil(t, client.Options().BaseEndpoint, "real AWS endpoint resolution")
}

// LoadDefaultConfig fails when the shared config file it is pointed at for
// an explicitly named profile does not define that profile.
func TestNewSQSClient_AWSConfigErrorIsWrapped(t *testing.T) {
	t.Setenv("AWS_PROFILE", "t4-no-such-profile")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/config")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/credentials")

	client, err := newSQSClient(t.Context(), eventcfg.SQSConfigEnv{Region: "us-east-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "load aws config")
	assert.Nil(t, client)
}
