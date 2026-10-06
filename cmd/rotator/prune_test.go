package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

type fakeOutboxPruner struct {
	n   int64
	err error
}

func (f *fakeOutboxPruner) PrunePublished(context.Context, time.Duration, int) (int64, error) {
	return f.n, f.err
}

func TestRunPrune_SummarizesBothHalves(t *testing.T) {
	processed := &fakeInboxPruner{pruneN: 3}
	outboxPruner := &fakeOutboxPruner{n: 7}

	result := runPrune(context.Background(), processed, outboxPruner, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, int64(3), result.ProcessedEventsDeleted)
	assert.Equal(t, port.ProcessedEventsConsumerTenantOffboarding, processed.consumer)
	assert.Equal(t, 8*24*time.Hour, processed.retention, "PROCESSED_EVENTS_TTL_DAYS becomes the inbox retention")
	assert.Equal(t, 1000, processed.batch)
	assert.Equal(t, int64(7), result.OutboxDeleted)
}

func TestRunPrune_OneHalfFailingDoesNotZeroTheOther(t *testing.T) {
	processed := &fakeInboxPruner{pruneN: 5}
	outboxPruner := &fakeOutboxPruner{err: errors.New("db unavailable")}

	result := runPrune(context.Background(), processed, outboxPruner, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, int64(5), result.ProcessedEventsDeleted)
	assert.Equal(t, int64(0), result.OutboxDeleted)
}

func TestRunPrune_ProcessedEventsFailureDoesNotZeroOutbox(t *testing.T) {
	processed := &fakeInboxPruner{pruneErr: errors.New("db unavailable")}
	outboxPruner := &fakeOutboxPruner{n: 9}

	result := runPrune(context.Background(), processed, outboxPruner, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, int64(0), result.ProcessedEventsDeleted)
	assert.Equal(t, int64(9), result.OutboxDeleted)
}

type fakeRLSViolationPruner struct {
	n              int
	err            error
	ttlDays, limit int
}

func (f *fakeRLSViolationPruner) Prune(_ context.Context, ttlDays, limit int) (int, error) {
	f.ttlDays, f.limit = ttlDays, limit
	return f.n, f.err
}

func TestRunRLSViolationPrune(t *testing.T) {
	t.Run("returns the deleted count and passes the retention through", func(t *testing.T) {
		p := &fakeRLSViolationPruner{n: 4}
		deleted, failed := runRLSViolationPrune(context.Background(), p, 30, 1000, nil)
		assert.Equal(t, 4, deleted)
		assert.False(t, failed)
		assert.Equal(t, 30, p.ttlDays)
		assert.Equal(t, 1000, p.limit)
	})
	t.Run("a failure is reported as zero deleted and fails the run", func(t *testing.T) {
		p := &fakeRLSViolationPruner{n: 4, err: errors.New("db down")}
		deleted, failed := runRLSViolationPrune(context.Background(), p, 30, 1000, nil)
		assert.Equal(t, 0, deleted)
		assert.True(t, failed, "a failed prune fails the run")
	})
}

func TestRunPrune_SignalledRunDefersBothStepsWithoutFailing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := runPrune(ctx, &fakeInboxPruner{pruneN: 3}, &fakeOutboxPruner{n: 7}, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, pruneResult{Deferred: 2}, result)
}

// A step the run deadline cuts off mid-way (its in-flight batch rolled
// back) is Deferred, not Failed — it must not fail the Job.
func TestRunPrune_StepCutOffByBudgetIsDeferred(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outboxPruner := &cancellingOutboxPruner{cancel: cancel}

	result := runPrune(ctx, &fakeInboxPruner{pruneN: 3}, outboxPruner, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, pruneResult{ProcessedEventsDeleted: 3, Deferred: 1}, result)
}

type cancellingOutboxPruner struct{ cancel context.CancelFunc }

func (c *cancellingOutboxPruner) PrunePublished(ctx context.Context, _ time.Duration, _ int) (int64, error) {
	c.cancel()
	return 0, ctx.Err()
}

type seqRLSViolationPruner struct {
	seq   []int
	calls int
}

func (s *seqRLSViolationPruner) Prune(context.Context, int, int) (int, error) {
	n := s.seq[s.calls]
	s.calls++
	return n, nil
}

func TestRunRLSViolationPrune_RepeatsWhileBatchesAreFull(t *testing.T) {
	p := &seqRLSViolationPruner{seq: []int{10, 10, 3}}

	deleted, failed := runRLSViolationPrune(context.Background(), p, 30, 10, nil)

	assert.False(t, failed)
	assert.Equal(t, 23, deleted)
	assert.Equal(t, 3, p.calls)
}

func TestRunRLSViolationPrune_OutOfBudgetIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), pruneReserve/2)
	defer cancel()
	p := &seqRLSViolationPruner{}

	deleted, failed := runRLSViolationPrune(ctx, p, 30, 10, nil)

	assert.False(t, failed)
	assert.Equal(t, 0, deleted)
	assert.Equal(t, 0, p.calls)
}
