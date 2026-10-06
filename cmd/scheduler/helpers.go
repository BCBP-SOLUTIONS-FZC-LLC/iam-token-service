package main

import (
	"context"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// startJobSpan starts a top-level scheduler span on gincommon's
// process-global TracerProvider — mirrors cmd/rotator's identical helper.
func startJobSpan(ctx context.Context, serviceName, jobName string) (context.Context, func()) {
	if serviceName == "" {
		serviceName = "iam-token-service"
	}
	ctx, span := gincommon.NewTracer(serviceName).Start(ctx, "scheduler."+jobName)
	return ctx, func() { span.End() }
}

// recordCadenceScanMetric records iam_token_service_cadence_rotation_total{result}
// (§16 TSQ-6 Resolved, §11.2) once per outcome observed in the run —
// mirrors cmd/rotator's recordSweepMetric/recordMaterialReconcileMetric
// per-outcome-not-just-per-run shape, since "rotated" and "failed" are
// both independently meaningful (a failed cadence rotation is page-worthy:
// it means Keycloak may now be out of sync with this service's own
// record, §8.7-style two-halves risk).
func recordCadenceScanMetric(result cadenceScanResult) {
	if result.Rotated > 0 {
		metrics.CadenceRotationTotal.WithLabelValues("rotated").Add(float64(result.Rotated))
	}
	if result.Skipped > 0 {
		metrics.CadenceRotationTotal.WithLabelValues("skipped").Add(float64(result.Skipped))
	}
	if result.Failed > 0 {
		metrics.CadenceRotationTotal.WithLabelValues("failed").Add(float64(result.Failed))
	}
}

// withTenantGUC binds the RLS GUC to tenantID + the reserved system
// principal (§4.3) — every IssueOrRotate call this job makes is scoped to
// exactly one principal's own tenant, never BYPASSRLS (RLS-7). Mirrors
// cmd/rotator's identical helper.
func withTenantGUC(ctx context.Context, tenantID uuid.UUID) context.Context {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.UserID = domain.SystemPrincipalID.String()
	g.TenantID = tenantID.String()
	return pgcommon.WithGUCSet(ctx, g)
}

// fieldsWithTrace copies fields and, when ctx carries a valid span from
// gincommon's TracerProvider, adds trace_id — mirrors cmd/rotator's
// identical helper.
func fieldsWithTrace(ctx context.Context, fields map[string]interface{}) map[string]interface{} {
	if traceID := gincommon.SpanTraceID(ctx); traceID != "" {
		if fields == nil {
			fields = map[string]interface{}{}
		}
		fields["trace_id"] = traceID
	}
	return fields
}

// logError is a nil-safe port.Logger.Error call — mirrors cmd/rotator's
// identical helper.
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
