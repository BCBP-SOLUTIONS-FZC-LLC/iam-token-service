package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

type fakeRP struct{ err error }

func (f fakeRP) RefreshKeys(context.Context, uuid.UUID) error { return f.err }

func TestInstrumentedRealmProvisionerClient_RecordsOutcome(t *testing.T) {
	Register("test")
	ok := DependencyRequestDuration.WithLabelValues(DependencyRealmProvisioner, OperationRefreshKeys, OutcomeSuccess)
	failed := DependencyRequestDuration.WithLabelValues(DependencyRealmProvisioner, OperationRefreshKeys, OutcomeError)
	beforeOK, beforeFailed := histogramSampleCount(t, ok), histogramSampleCount(t, failed)

	assert.NoError(t, NewInstrumentedRealmProvisionerClient(fakeRP{}).RefreshKeys(context.Background(), uuid.New()))
	assert.Error(t, NewInstrumentedRealmProvisionerClient(fakeRP{err: errors.New("503")}).RefreshKeys(context.Background(), uuid.New()))

	assert.Equal(t, beforeOK+1, histogramSampleCount(t, ok))
	assert.Equal(t, beforeFailed+1, histogramSampleCount(t, failed))
}
