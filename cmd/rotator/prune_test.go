package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type fakeOutboxPruner struct {
	n   int64
	err error
}

func (f *fakeOutboxPruner) PrunePublished(context.Context, time.Duration, int) (int64, error) {
	return f.n, f.err
}

func TestRunPrune_SummarizesBothHalves(t *testing.T) {
	processed := &fakeProcessedEventsStore{pruneN: 3}
	outboxPruner := &fakeOutboxPruner{n: 7}

	result := runPrune(context.Background(), processed, outboxPruner, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, 3, result.ProcessedEventsDeleted)
	assert.Equal(t, int64(7), result.OutboxDeleted)
}

func TestRunPrune_OneHalfFailingDoesNotZeroTheOther(t *testing.T) {
	processed := &fakeProcessedEventsStore{pruneN: 5}
	outboxPruner := &fakeOutboxPruner{err: errors.New("db unavailable")}

	result := runPrune(context.Background(), processed, outboxPruner, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, 5, result.ProcessedEventsDeleted)
	assert.Equal(t, int64(0), result.OutboxDeleted)
}

func TestRunPrune_ProcessedEventsFailureDoesNotZeroOutbox(t *testing.T) {
	processed := &fakeProcessedEventsStore{pruneErr: errors.New("db unavailable")}
	outboxPruner := &fakeOutboxPruner{n: 9}

	result := runPrune(context.Background(), processed, outboxPruner, 8, 1000, 7*24*time.Hour, nil)

	assert.Equal(t, 0, result.ProcessedEventsDeleted)
	assert.Equal(t, int64(9), result.OutboxDeleted)
}
