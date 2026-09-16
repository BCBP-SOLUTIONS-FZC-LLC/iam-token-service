package main

import (
	"context"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// startJobSpan starts a top-level rotator span on gincommon's process-global
// TracerProvider (installed by InitTracingFromEnv in main). Matches
// iam-org-membership cmd/reconciler's otel.Tracer(serviceName).Start.
func startJobSpan(ctx context.Context, serviceName, jobName string) (context.Context, func()) {
	if serviceName == "" {
		serviceName = "iam-token-service"
	}
	ctx, span := otel.Tracer(serviceName).Start(ctx, "rotator."+jobName)
	return ctx, func() { span.End() }
}

// recordSweepMetric records iam_token_service_rotation_sweep_total{result}
// (§8.3, §11.2) once per run — "ok" when nothing failed, "error" otherwise
// (a partial failure still logs individually via revokeExpiredRotating's
// callers; this is the run-level summary the metric's frozen label set
// describes).
func recordSweepMetric(result sweepResult) {
	label := "ok"
	if result.Failed > 0 {
		label = "error"
	}
	metrics.RotationSweepTotal.WithLabelValues(label).Inc()
}

// recordMaterialReconcileMetric records
// iam_token_service_material_reconcile_total{result} (§8.6, §11.2) once
// per outcome observed in the run — an orphan_deleted count and a
// missing_material count are independently meaningful (the latter is
// page-worthy, §11.5), so both are recorded when non-zero, alongside a
// single ok/error summary for the rest.
func recordMaterialReconcileMetric(result materialReconcileResult) {
	if result.OrphanDeleted > 0 {
		metrics.MaterialReconcileTotal.WithLabelValues("orphan_deleted").Add(float64(result.OrphanDeleted))
	}
	if result.MissingMaterial > 0 {
		metrics.MaterialReconcileTotal.WithLabelValues("missing_material").Add(float64(result.MissingMaterial))
	}
	if result.Failed > 0 {
		metrics.MaterialReconcileTotal.WithLabelValues("error").Add(float64(result.Failed))
	}
	if result.OK > 0 {
		metrics.MaterialReconcileTotal.WithLabelValues("ok").Add(float64(result.OK))
	}
}

// withTenantGUC binds the RLS GUC to tenantID + the reserved system
// principal (§4.3) — every write this job performs is scoped to exactly
// one row's own tenant, never BYPASSRLS (RLS-7).
func withTenantGUC(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = domain.SystemPrincipalID.String()
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// fieldsWithTrace copies fields and, when ctx carries a valid span from
// gincommon's TracerProvider, adds trace_id so rotator logs join the same
// trace as the rotator.* job span (iam-user-profile logCtx).
func fieldsWithTrace(ctx context.Context, fields map[string]interface{}) map[string]interface{} {
	if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		if fields == nil {
			fields = map[string]interface{}{}
		}
		fields["trace_id"] = span.SpanContext().TraceID().String()
	}
	return fields
}

// logError is a nil-safe port.Logger.Error call — every job function
// accepts a possibly-nil Logger (tests pass nil) rather than requiring a
// no-op logger implementation.
func logError(ctx context.Context, log port.Logger, msg string, fields map[string]interface{}, err error) {
	if log == nil {
		return
	}
	if fields == nil {
		fields = map[string]interface{}{}
	}
	fields["error"] = err.Error()
	log.Error(msg, fieldsWithTrace(ctx, fields))
}
