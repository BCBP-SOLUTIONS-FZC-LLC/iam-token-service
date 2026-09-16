package postgres

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// otelTracer adapts the process-global TracerProvider that
// gincommon.InitTracingFromEnv / ObservabilityMiddlewares install onto
// pgcommon.Config.Tracer (StartSpan). Every db.query span therefore
// exports through the same OTLP pipeline as HTTP spans — matching the
// sibling iam-org-membership / iam-user-profile wiring.
type otelTracer struct {
	tracer trace.Tracer
}

// NewOTelTracer returns a pgcommon.Config.Tracer backed by gincommon's
// global TracerProvider. serviceName becomes the OTel instrumentation
// scope (typically gincommon.Config.ServiceName).
func NewOTelTracer(serviceName string) *otelTracer {
	if serviceName == "" {
		serviceName = "iam-token-service"
	}
	return &otelTracer{tracer: otel.Tracer(serviceName)}
}

func (o *otelTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	ctx, span := o.tracer.Start(ctx, name)
	return ctx, func() { span.End() }
}
