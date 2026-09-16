package eventbus

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

// fakeFieldLogger implements port.Logger, recording the fields map passed to
// each call (not just the message) — needed to assert on the trace_id
// stamped by a valid span context, which fakeWarnLogger (message-only)
// cannot observe.
type fakeFieldLogger struct {
	debugCalls []map[string]interface{}
	warnCalls  []map[string]interface{}
}

func (f *fakeFieldLogger) Debug(_ string, fields map[string]interface{}) {
	f.debugCalls = append(f.debugCalls, fields)
}
func (f *fakeFieldLogger) Info(string, map[string]interface{}) {}
func (f *fakeFieldLogger) Warn(_ string, fields map[string]interface{}) {
	f.warnCalls = append(f.warnCalls, fields)
}
func (f *fakeFieldLogger) Error(string, map[string]interface{}) {}

// TestGlueCodec_Warn_ValidSpanAddsTraceID covers warn's trace_id-stamping
// branch directly (white-box) — StartRefresher never carries a span on its
// background ctx, so this branch is unreachable through that entry point.
func TestGlueCodec_Warn_ValidSpanAddsTraceID(t *testing.T) {
	fl := &fakeFieldLogger{}
	g := &GlueCodec{log: fl}

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	ctx := trace.ContextWithSpanContext(t.Context(), sc)

	g.warn(ctx, "test warn", "SchemaX", errors.New("boom"))
	require.Len(t, fl.warnCalls, 1)
	assert.Equal(t, traceID.String(), fl.warnCalls[0]["trace_id"])
	assert.Equal(t, "SchemaX", fl.warnCalls[0]["schema"])
}

// TestGlueCodec_Warn_NilLoggerDoesNotPanic covers warn's nil-logger guard
// directly.
func TestGlueCodec_Warn_NilLoggerDoesNotPanic(t *testing.T) {
	g := &GlueCodec{}
	assert.NotPanics(t, func() { g.warn(t.Context(), "test warn", "SchemaX", errors.New("boom")) })
}
