// Package traceutil provides shared tracing helpers for log-field enrichment.
package traceutil

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// WithTraceID adds the active span's trace_id to a log-fields map if the span
// context is valid. If fields is nil and a trace ID is available, a new map is
// allocated. Returns the (possibly new) map for chaining.
func WithTraceID(ctx context.Context, fields map[string]any) map[string]any {
	sc := trace.SpanFromContext(ctx).SpanContext()
	if !sc.IsValid() {
		return fields
	}
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["trace_id"] = sc.TraceID().String()
	return fields
}
