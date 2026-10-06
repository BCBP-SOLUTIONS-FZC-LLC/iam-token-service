package main

import (
	"context"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
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
	ctx, span := gincommon.NewTracer("iam-token-service").Start(ctx, "exporter.rotation_overlap")
	defer span.End()

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var count int
	err := reconcilerPool.WithConn(queryCtx, func(ctx context.Context, conn *pgxpool.Conn) error {
		return conn.QueryRow(ctx, `SELECT count(*) FROM service_account_credentials WHERE status = 'rotating' AND expires_at IS NOT NULL AND deleted_at IS NULL`).Scan(&count)
	})
	if err != nil {
		if log != nil {
			fields := map[string]interface{}{"error": err.Error()}
			if traceID := gincommon.SpanTraceID(ctx); traceID != "" {
				fields["trace_id"] = traceID
			}
			log.Warn("rotation overlap exporter: query failed", fields)
		}
		return
	}
	metrics.RotationOverlapActive.Set(float64(count))
}

// rlsViolationCounter is the slice of pgadapter.RLSViolationRepository the
// RLS exporter needs (a local interface so tests can fake it).
type rlsViolationCounter interface {
	CursorBefore(ctx context.Context, window time.Duration) (int64, error)
	CountSince(ctx context.Context, afterID int64) (map[string]int64, int64, error)
}

// runRLSViolationExporter turns new rls_violation_log rows (migration 000003)
// into iam_rls_violations_total{violation_type} every interval, over the
// BYPASSRLS reconciler pool — the same pattern as iam-org-membership's
// rls_violations exporter, but with an id cursor instead of a time window,
// so a row is counted exactly once per pod even when a tick is late. The
// cursor starts one interval back, so a restarted pod re-counts at most the
// last interval rather than the table's whole history. Every server replica
// counts the same rows: alert on increase() > 0 and use max, not sum,
// across pods for magnitudes. Runs until ctx is cancelled.
func runRLSViolationExporter(ctx context.Context, repo rlsViolationCounter, interval time.Duration, log port.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	e := &rlsViolationExporter{repo: repo, window: interval, log: log}
	e.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

type rlsViolationExporter struct {
	repo        rlsViolationCounter
	window      time.Duration
	log         port.Logger
	cursor      int64
	initialized bool
}

func (e *rlsViolationExporter) tick(ctx context.Context) {
	ctx, span := gincommon.NewTracer("iam-token-service").Start(ctx, "exporter.rls_violations")
	defer span.End()

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if !e.initialized {
		cursor, err := e.repo.CursorBefore(queryCtx, e.window)
		if err != nil {
			e.warn(ctx, err)
			return
		}
		e.cursor, e.initialized = cursor, true
	}
	counts, cursor, err := e.repo.CountSince(queryCtx, e.cursor)
	if err != nil {
		e.warn(ctx, err)
		return
	}
	e.cursor = cursor
	for vType, n := range counts {
		metrics.AddRLSViolations(vType, float64(n))
	}
}

func (e *rlsViolationExporter) warn(ctx context.Context, err error) {
	if e.log == nil {
		return
	}
	fields := map[string]interface{}{"error": err.Error()}
	if traceID := gincommon.SpanTraceID(ctx); traceID != "" {
		fields["trace_id"] = traceID
	}
	e.log.Warn("rls violation exporter: query failed", fields)
}

// keysRefreshPendingStats is the slice of pgadapter.KeysRefreshRepository
// the keys_refresh_pending exporter needs (a local interface so tests can
// fake it).
type keysRefreshPendingStats interface {
	PendingStats(ctx context.Context) (int, time.Duration, error)
}

// runKeysRefreshPendingExporter refreshes
// iam_token_service_keys_refresh_pending and
// iam_token_service_keys_refresh_oldest_age_seconds every interval. A row in
// keys_refresh_pending is a committed rotate/revoke whose RP-17 key-cache
// refresh has not yet been confirmed at Keycloak — the two-halves gap EXT-6
// has no TTL to close on its own — so only a DB-state gauge, read
// cross-tenant over the reconciler pool, can show it. Runs until ctx is
// cancelled.
func runKeysRefreshPendingExporter(ctx context.Context, repo keysRefreshPendingStats, interval time.Duration, log port.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	refreshKeysRefreshPendingGauges(ctx, repo, log)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshKeysRefreshPendingGauges(ctx, repo, log)
		}
	}
}

// refreshKeysRefreshPendingGauges leaves both gauges at their last value on
// a query error: dropping them to 0 would read as "nothing pending" and
// silently resolve the very alert that a degraded database should keep
// firing.
func refreshKeysRefreshPendingGauges(ctx context.Context, repo keysRefreshPendingStats, log port.Logger) {
	ctx, span := gincommon.NewTracer("iam-token-service").Start(ctx, "exporter.keys_refresh_pending")
	defer span.End()

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	count, oldestAge, err := repo.PendingStats(queryCtx)
	if err != nil {
		if log != nil {
			fields := map[string]interface{}{"error": err.Error()}
			if traceID := gincommon.SpanTraceID(ctx); traceID != "" {
				fields["trace_id"] = traceID
			}
			log.Warn("keys refresh pending exporter: query failed", fields)
		}
		return
	}
	metrics.SetKeysRefreshPending(count, oldestAge.Seconds())
}
