package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

type fakeSecretStoreForDecorator struct {
	writeErr, deleteErr error
	readVal             string
	readErr             error
	listVal             []string
	listErr             error
	writeCalls          int
	deleteCalls         int
	readCalls           int
	listCalls           int
}

func (f *fakeSecretStoreForDecorator) Write(context.Context, string, string) error {
	f.writeCalls++
	return f.writeErr
}
func (f *fakeSecretStoreForDecorator) Read(context.Context, string) (string, error) {
	f.readCalls++
	return f.readVal, f.readErr
}
func (f *fakeSecretStoreForDecorator) Delete(context.Context, string) error {
	f.deleteCalls++
	return f.deleteErr
}
func (f *fakeSecretStoreForDecorator) List(context.Context, string) ([]string, error) {
	f.listCalls++
	return f.listVal, f.listErr
}

var _ port.SecretStore = (*fakeSecretStoreForDecorator)(nil)

// histogramSampleCount reads a HistogramVec's current observation count for
// one label combination — the standard prometheus/client_golang technique
// for asserting "an Observe happened" without depending on the exact bucket
// distribution.
func histogramSampleCount(t *testing.T, h prometheus.Observer) uint64 {
	t.Helper()
	collector, ok := h.(prometheus.Metric)
	require.True(t, ok, "expected a concrete prometheus.Metric from WithLabelValues")
	var m dto.Metric
	require.NoError(t, collector.Write(&m))
	require.NotNil(t, m.Histogram)
	return m.Histogram.GetSampleCount()
}

func TestInstrumentedSecretStore_Write_ObservesDurationAndPassesThrough(t *testing.T) {
	Register("test")
	before := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("write"))
	beforeDep := histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "write", "success"))

	inner := &fakeSecretStoreForDecorator{}
	s := NewInstrumentedSecretStore(inner)
	require.NoError(t, s.Write(context.Background(), "iam/serviceaccount/x/y/v1", "secret-value"))
	assert.Equal(t, 1, inner.writeCalls)

	after := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("write"))
	assert.Equal(t, before+1, after, "Write must observe exactly one sample on op=write")
	assert.Equal(t, beforeDep+1, histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "write", "success")),
		"the Tier-1 platform_dependency_request_seconds{dependency=openbao,operation=write,outcome=success} must be dual-emitted alongside the legacy metric")
}

func TestInstrumentedSecretStore_Write_ObservesDurationEvenOnError(t *testing.T) {
	Register("test")
	before := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("write"))
	beforeDep := histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "write", "error"))

	wantErr := errors.New("openbao unavailable")
	inner := &fakeSecretStoreForDecorator{writeErr: wantErr}
	s := NewInstrumentedSecretStore(inner)
	err := s.Write(context.Background(), "p", "v")
	require.ErrorIs(t, err, wantErr)

	after := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("write"))
	assert.Equal(t, before+1, after, "call duration is observed unconditionally, even on failure")
	assert.Equal(t, beforeDep+1, histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "write", "error")))
}

func TestInstrumentedSecretStore_Delete_ObservesDurationAndPassesThrough(t *testing.T) {
	Register("test")
	before := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("delete"))
	beforeDep := histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "delete", "success"))

	inner := &fakeSecretStoreForDecorator{}
	s := NewInstrumentedSecretStore(inner)
	require.NoError(t, s.Delete(context.Background(), "p"))
	assert.Equal(t, 1, inner.deleteCalls)

	after := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("delete"))
	assert.Equal(t, before+1, after)
	assert.Equal(t, beforeDep+1, histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "delete", "success")),
		"the Tier-1 platform_dependency_request_seconds{dependency=openbao,operation=delete,outcome=success} must be dual-emitted alongside the legacy metric")
}

func TestInstrumentedSecretStore_Delete_ObservesDurationEvenOnError(t *testing.T) {
	Register("test")
	before := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("delete"))
	beforeDep := histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "delete", "error"))

	wantErr := errors.New("openbao unavailable")
	inner := &fakeSecretStoreForDecorator{deleteErr: wantErr}
	s := NewInstrumentedSecretStore(inner)
	err := s.Delete(context.Background(), "p")
	require.ErrorIs(t, err, wantErr)

	after := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("delete"))
	assert.Equal(t, before+1, after)
	assert.Equal(t, beforeDep+1, histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "delete", "error")))
}

// TestInstrumentedSecretStore_Read_PassesThroughUnmeasured and
// TestInstrumentedSecretStore_List_PassesThroughUnmeasured confirm Read/List
// are NOT instrumented (per the package doc comment — only write|delete are
// in the frozen op label set) — they must not touch OpenBaoCallDuration at
// all, on either op label, success or failure.
func TestInstrumentedSecretStore_Read_PassesThroughUnmeasured(t *testing.T) {
	Register("test")
	beforeWrite := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("write"))
	beforeDelete := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("delete"))
	beforeDepWrite := histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "write", "success"))
	beforeDepDelete := histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "delete", "success"))

	inner := &fakeSecretStoreForDecorator{readVal: "plaintext"}
	s := NewInstrumentedSecretStore(inner)
	got, err := s.Read(context.Background(), "p")
	require.NoError(t, err)
	assert.Equal(t, "plaintext", got)
	assert.Equal(t, 1, inner.readCalls)

	assert.Equal(t, beforeWrite, histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("write")))
	assert.Equal(t, beforeDelete, histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("delete")))
	assert.Equal(t, beforeDepWrite, histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "write", "success")))
	assert.Equal(t, beforeDepDelete, histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "delete", "success")))
}

func TestInstrumentedSecretStore_Read_ErrorPassesThroughUnmeasured(t *testing.T) {
	Register("test")
	wantErr := errors.New("no secret at path")
	inner := &fakeSecretStoreForDecorator{readErr: wantErr}
	s := NewInstrumentedSecretStore(inner)
	_, err := s.Read(context.Background(), "p")
	require.ErrorIs(t, err, wantErr)
}

func TestInstrumentedSecretStore_List_PassesThroughUnmeasured(t *testing.T) {
	Register("test")
	beforeWrite := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("write"))
	beforeDelete := histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("delete"))
	beforeDepWrite := histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "write", "success"))
	beforeDepDelete := histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "delete", "success"))

	inner := &fakeSecretStoreForDecorator{listVal: []string{"v1", "v2"}}
	s := NewInstrumentedSecretStore(inner)
	got, err := s.List(context.Background(), "prefix")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1", "v2"}, got)
	assert.Equal(t, 1, inner.listCalls)

	assert.Equal(t, beforeWrite, histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("write")))
	assert.Equal(t, beforeDelete, histogramSampleCount(t, OpenBaoCallDuration.WithLabelValues("delete")))
	assert.Equal(t, beforeDepWrite, histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "write", "success")))
	assert.Equal(t, beforeDepDelete, histogramSampleCount(t, DependencyRequestDuration.WithLabelValues("openbao", "delete", "success")))
}

func TestInstrumentedSecretStore_List_ErrorPassesThrough(t *testing.T) {
	Register("test")
	wantErr := errors.New("list failed")
	inner := &fakeSecretStoreForDecorator{listErr: wantErr}
	s := NewInstrumentedSecretStore(inner)
	_, err := s.List(context.Background(), "prefix")
	require.ErrorIs(t, err, wantErr)
}

func TestNewInstrumentedSecretStore_SatisfiesSecretStorePort(t *testing.T) {
	var _ port.SecretStore = NewInstrumentedSecretStore(&fakeSecretStoreForDecorator{})
}
