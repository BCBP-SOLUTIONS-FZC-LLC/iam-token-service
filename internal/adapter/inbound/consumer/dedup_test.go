package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

type stubDedup struct {
	seen    map[string]bool
	marked  []string
	err     error
	markErr error
}

func (d *stubDedup) IsProcessed(_ context.Context, _ port.ProcessedEventsConsumer, eventID string) (bool, error) {
	if d.err != nil {
		return false, d.err
	}
	return d.seen[eventID], nil
}

func (d *stubDedup) MarkProcessed(_ context.Context, _ port.ProcessedEventsConsumer, eventID string) error {
	if d.markErr != nil {
		return d.markErr
	}
	d.marked = append(d.marked, eventID)
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	d.seen[eventID] = true
	return nil
}

func (d *stubDedup) Prune(context.Context, int, int) (int, error) { return 0, nil }

type stubTx struct{}

func (stubTx) RunInTx(_ context.Context, fn func(context.Context) error) error {
	return fn(context.Background())
}

func TestSkipDuplicate_IncrementsMetricOnHit(t *testing.T) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
		ServiceName: "iam-token-service", BuildVersion: "test", Domain: "iam", Environment: "test",
	})
	metrics.Register("test")
	before := testutil.ToFloat64(metrics.ProcessedEventsDuplicates.WithLabelValues(string(port.ProcessedEventsConsumerTenantOffboarding)))
	beforePlatform := testutil.ToFloat64(metrics.DuplicateMessagesTotal.WithLabelValues(queueNameForConsumer(port.ProcessedEventsConsumerTenantOffboarding), "TenantMembershipsPurged"))
	dedup := &stubDedup{seen: map[string]bool{"evt-1": true}}

	hit, err := skipDuplicate(context.Background(), dedup, port.ProcessedEventsConsumerTenantOffboarding, "evt-1", "TenantMembershipsPurged")
	require.NoError(t, err)
	assert.True(t, hit)
	assert.InDelta(t, before+1, testutil.ToFloat64(metrics.ProcessedEventsDuplicates.WithLabelValues(string(port.ProcessedEventsConsumerTenantOffboarding))), 0.01)
	assert.InDelta(t, beforePlatform+1, testutil.ToFloat64(metrics.DuplicateMessagesTotal.WithLabelValues(queueNameForConsumer(port.ProcessedEventsConsumerTenantOffboarding), "TenantMembershipsPurged")), 0.01,
		"the Tier-1 platform_duplicate_messages_total{queue,event_type} must be dual-emitted alongside the legacy metric")

	hit, err = skipDuplicate(context.Background(), dedup, port.ProcessedEventsConsumerTenantOffboarding, "evt-new", "TenantMembershipsPurged")
	require.NoError(t, err)
	assert.False(t, hit)
}

func TestQueueNameForConsumer(t *testing.T) {
	t.Run("known consumer maps to its queue name", func(t *testing.T) {
		assert.Equal(t, "tenant-lifecycle-tokensvc-q", queueNameForConsumer(port.ProcessedEventsConsumerTenantOffboarding))
	})
	t.Run("unknown consumer falls back to its own string value", func(t *testing.T) {
		assert.Equal(t, "some_future_consumer", queueNameForConsumer(port.ProcessedEventsConsumer("some_future_consumer")))
	})
}

func TestAckUnknown_MarksProcessed(t *testing.T) {
	dedup := &stubDedup{}
	env := events.Envelope[json.RawMessage]{ID: "evt-unknown", Type: "NeverHeardOfThis"}
	err := ackUnknown(context.Background(), stubTx{}, dedup, nil, port.ProcessedEventsConsumerTenantOffboarding, env)
	require.NoError(t, err)
	assert.Equal(t, []string{"evt-unknown"}, dedup.marked)
}

func TestSkipDuplicate_IsProcessedErrorPropagates(t *testing.T) {
	wantErr := errors.New("dedup store unavailable")
	dedup := &stubDedup{err: wantErr}

	hit, err := skipDuplicate(context.Background(), dedup, port.ProcessedEventsConsumerTenantOffboarding, "evt-x", "TenantMembershipsPurged")
	require.ErrorIs(t, err, wantErr)
	assert.False(t, hit)
}

func TestMarkProcessedInTx_WrapsMarkProcessedError(t *testing.T) {
	wantErr := errors.New("insert conflict")
	dedup := &stubDedup{markErr: wantErr}

	err := markProcessedInTx(context.Background(), stubTx{}, dedup, port.ProcessedEventsConsumerTenantOffboarding, "evt-y")
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
	assert.Contains(t, err.Error(), "mark processed")
}
