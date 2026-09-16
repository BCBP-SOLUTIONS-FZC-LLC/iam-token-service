package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// runRotationOverlapExporter refreshes iam_token_service_rotation_overlap_active
// (§11.2) every interval from the BYPASSRLS reconciler pool — a
// cross-tenant count of `rotating` credentials, which no per-request or
// per-tenant path can maintain. Runs until ctx is cancelled.
func runRotationOverlapExporter(ctx context.Context, reconcilerPool *pgcommon.Pool, interval time.Duration, log port.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	refreshRotationOverlapGauge(ctx, reconcilerPool, log)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshRotationOverlapGauge(ctx, reconcilerPool, log)
		}
	}
}

func refreshRotationOverlapGauge(ctx context.Context, reconcilerPool *pgcommon.Pool, log port.Logger) {
	ctx, span := otel.Tracer("iam-token-service").Start(ctx, "exporter.rotation_overlap")
	defer span.End()

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var count int
	err := reconcilerPool.WithConn(queryCtx, func(ctx context.Context, conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx, `SELECT count(*) FROM service_account_credentials WHERE status = 'rotating' AND deleted_at IS NULL`).Scan(&count)
	})
	if err != nil {
		if log != nil {
			fields := map[string]interface{}{"error": err.Error()}
			if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
				fields["trace_id"] = spanCtx.TraceID().String()
			}
			log.Warn("rotation overlap exporter: query failed", fields)
		}
		return
	}
	metrics.RotationOverlapActive.Set(float64(count))
}
