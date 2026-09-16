package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestWithTraceID_BackgroundContextOmitsTraceID(t *testing.T) {
	got := withTraceID(context.Background(), map[string]any{"k": "v"})
	assert.Equal(t, map[string]any{"k": "v"}, got)
}

func TestWithTraceID_NilFieldsWithNoSpan(t *testing.T) {
	assert.Nil(t, withTraceID(context.Background(), nil))
}

func validSpanContextForTracingTest(t *testing.T) (context.Context, oteltrace.TraceID) {
	t.Helper()
	traceID, err := oteltrace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := oteltrace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: oteltrace.FlagsSampled})
	return oteltrace.ContextWithSpanContext(context.Background(), sc), traceID
}

func TestWithTraceID_ValidSpanAddsTraceID(t *testing.T) {
	ctx, traceID := validSpanContextForTracingTest(t)
	got := withTraceID(ctx, map[string]any{"k": "v"})
	assert.Equal(t, traceID.String(), got["trace_id"])
	assert.Equal(t, "v", got["k"])
}

func TestWithTraceID_ValidSpanWithNilFieldsInitializesMap(t *testing.T) {
	ctx, traceID := validSpanContextForTracingTest(t)
	got := withTraceID(ctx, nil)
	assert.Equal(t, traceID.String(), got["trace_id"])
}
