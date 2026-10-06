package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// stubInbox is an in-memory port.Inbox: an ID runs fn once, then is a
// duplicate; an fn error leaves the ID unclaimed (the real claim rolls back).
type stubInbox struct {
	seen map[string]bool
	err  error
}

func (i *stubInbox) ProcessOnce(ctx context.Context, _ port.ProcessedEventsConsumer, eventID, _ string, fn func(context.Context) error) (bool, error) {
	if i.err != nil {
		return false, i.err
	}
	if i.seen[eventID] {
		return true, nil
	}
	if err := fn(ctx); err != nil {
		return false, err
	}
	if i.seen == nil {
		i.seen = map[string]bool{}
	}
	i.seen[eventID] = true
	return false, nil
}

func envelope(id, eventType string) events.Envelope[json.RawMessage] {
	return events.Envelope[json.RawMessage]{ID: id, Type: eventType}
}

func TestProcessOnce_DuplicateSkipsFnAndCountsTier3Only(t *testing.T) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName: "iam-token-service", BuildVersion: "test", Domain: "iam", Environment: "test",
	})
	metrics.Register("test")
	consumer := port.ProcessedEventsConsumerTenantOffboarding
	before := testutil.ToFloat64(metrics.ProcessedEventsDuplicates.WithLabelValues(string(consumer)))
	beforePlatform := testutil.ToFloat64(metrics.DuplicateMessagesTotal.WithLabelValues("tenant-lifecycle-tokensvc-q", "TenantMembershipsPurged"))
	inbox := &stubInbox{seen: map[string]bool{"evt-1": true}}

	runs := 0
	dup, err := processOnce(context.Background(), inbox, consumer, envelope("evt-1", "TenantMembershipsPurged"), func(context.Context) error { runs++; return nil })
	require.NoError(t, err)
	assert.True(t, dup)
	assert.Zero(t, runs)
	assert.InDelta(t, before+1, testutil.ToFloat64(metrics.ProcessedEventsDuplicates.WithLabelValues(string(consumer))), 0.01)
	assert.InDelta(t, beforePlatform, testutil.ToFloat64(metrics.DuplicateMessagesTotal.WithLabelValues("tenant-lifecycle-tokensvc-q", "TenantMembershipsPurged")), 0.01,
		"platform_duplicate_messages_total is counted by platform-events' inbox, never by the service (no double counting)")

	dup, err = processOnce(context.Background(), inbox, consumer, envelope("evt-new", "TenantMembershipsPurged"), func(context.Context) error { runs++; return nil })
	require.NoError(t, err)
	assert.False(t, dup)
	assert.Equal(t, 1, runs)
}

func TestProcessOnce_ErrorPropagates(t *testing.T) {
	wantErr := errors.New("inbox unavailable")
	dup, err := processOnce(context.Background(), &stubInbox{err: wantErr}, port.ProcessedEventsConsumerTenantOffboarding,
		envelope("evt-x", "TenantMembershipsPurged"), func(context.Context) error { return nil })
	require.ErrorIs(t, err, wantErr)
	assert.False(t, dup)
}

func TestBoundedEventType(t *testing.T) {
	assert.Equal(t, "TenantCreated", boundedEventType("TenantCreated"))
	assert.Equal(t, "TenantMembershipsPurged", boundedEventType("TenantMembershipsPurged"))
	// Well-formed but unknown names used to pass the old shape-only check
	// and each minted a new series.
	assert.Equal(t, "other", boundedEventType("iam.tenant.v2-created"))
	assert.Equal(t, "other", boundedEventType("NeverHeardOfThis"))
	assert.Equal(t, "other", boundedEventType(""))
	assert.Equal(t, "other", boundedEventType("<script>"))
	assert.Equal(t, "other", boundedEventType(strings.Repeat("A", 65)))
}

// recordingInbox captures the event type the inbox receives — it becomes
// platform_duplicate_messages_total's event_type label.
type recordingInbox struct{ eventType string }

func (i *recordingInbox) ProcessOnce(ctx context.Context, _ port.ProcessedEventsConsumer, _, eventType string, fn func(context.Context) error) (bool, error) {
	i.eventType = eventType
	return false, fn(ctx)
}

func TestAckUnknown_FoldsUnknownTypeIntoOther(t *testing.T) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName: "iam-token-service", BuildVersion: "test", Domain: "iam", Environment: "test",
	})
	metrics.Register("test")
	consumer := port.ProcessedEventsConsumerTenantOffboarding
	before := testutil.ToFloat64(metrics.UnknownEventAcknowledged.WithLabelValues(string(consumer), "other"))
	inbox := &recordingInbox{}

	require.NoError(t, ackUnknown(context.Background(), inbox, nil, consumer, envelope("evt-u1", "BrandNewProducerEvent")))
	assert.InDelta(t, before+1, testutil.ToFloat64(metrics.UnknownEventAcknowledged.WithLabelValues(string(consumer), "other")), 0.01)
	assert.Equal(t, "other", inbox.eventType, "the inbox's duplicate metric gets the bounded type too")
	assert.Zero(t, testutil.ToFloat64(metrics.UnknownEventAcknowledged.WithLabelValues(string(consumer), "BrandNewProducerEvent")),
		"no per-type series for an unknown name")
}

func TestAckUnknown_RecordsTheEvent(t *testing.T) {
	inbox := &stubInbox{}
	err := ackUnknown(context.Background(), inbox, nil, port.ProcessedEventsConsumerTenantOffboarding, envelope("evt-unknown", "NeverHeardOfThis"))
	require.NoError(t, err)
	assert.True(t, inbox.seen["evt-unknown"], "an unknown type is claimed so redelivery does not storm it")
}
