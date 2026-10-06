package main

import (
	"context"
	"time"

	"github.com/google/uuid"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// keysRefreshMarkers is the keys_refresh_pending capability (migration
// 000005) this job needs — *pgadapter.KeysRefreshRepository satisfies it;
// unit tests fake it. The marker is the durable record that an RP-17
// key-cache refresh is owed for a tenant: the scan writes an intent marker
// (MarkIntent) in its own transaction BEFORE each IssueOrRotate
// (intent-first — a spurious extra RP-17 is harmless, a missed one is not),
// re-writes it as committed (MarkPending) once IssueOrRotate returns, and
// clears it only after RP-17 succeeds, so a failed call, a run-deadline
// kill or a SIGKILL between the commit and the refresh is retried by the
// next run instead of leaving Keycloak on a stale key set indefinitely
// (EXT-6 has no key-cache TTL, TS-INV-7). Mirrors cmd/rotator's file of the
// same name (separate main packages cannot share it); only this job writes
// intent markers.
type keysRefreshMarkers interface {
	MarkIntent(ctx context.Context, tenantID uuid.UUID, ttl time.Duration) (pgadapter.KeysRefreshMark, error)
	MarkPending(ctx context.Context, tenantID uuid.UUID) (pgadapter.KeysRefreshMark, error)
	Clear(ctx context.Context, tenantID uuid.UUID, upTo time.Time) error
	ListPending(ctx context.Context, limit int) ([]pgadapter.PendingKeysRefresh, error)
}

// pendingRefreshResult summarizes the start-of-run retry pass.
type pendingRefreshResult struct {
	Refreshed int // RP-17 succeeded and the marker was cleared
	Failed    int // RP-17 failed (the marker stays for the next run), or the listing failed
	Dropped   int // RP-17 failed for a tenant with no principal left (offboarded) — marker dropped, not paged on forever
	Deferred  int // not attempted: the pass's share of the run budget was spent
}

// retryPendingKeyRefreshes calls RP-17 for every tenant whose marker a
// previous run left behind (its RP-17 failed, or the run was killed before
// making it), clearing each marker on success. ListPending returns committed
// markers at once and cmd/scheduler's intent markers only once their
// intent_until has passed (their writer finished or died), so a sweep's
// committed revoke is no longer held back by the intent guard. It gets at most half of the
// run's remaining budget so an RP-17 outage — every call then taking up to
// rp17Timeout — cannot starve this run's own scan.
func retryPendingKeyRefreshes(ctx context.Context, markers keysRefreshMarkers, rp port.RealmProvisionerClient, limit int, log port.Logger) pendingRefreshResult {
	var result pendingRefreshResult
	pending, err := markers.ListPending(ctx, limit)
	if err != nil {
		result.Failed++
		logError(ctx, log, "keys refresh retry: list pending markers failed", nil, err)
		return result
	}
	phaseEnd, bounded := time.Time{}, false
	if deadline, ok := ctx.Deadline(); ok {
		phaseEnd, bounded = time.Now().Add(time.Until(deadline)/2), true
	}
	for i, p := range pending {
		if ctx.Err() != nil || (bounded && time.Until(phaseEnd) < rp17Timeout) {
			result.Deferred = len(pending) - i
			if log != nil {
				log.Warn("keys refresh retry: budget spent — leaving the remaining markers to the next run", fieldsWithTrace(ctx, map[string]interface{}{"deferred": result.Deferred}))
			}
			break
		}
		err := refreshAndClear(ctx, markers, rp, p.TenantID, p.RequestedAt, log)
		switch {
		case err == nil:
			result.Refreshed++
		case !p.HasPrincipal:
			// Offboarded since the marker was written: its Keycloak client is
			// gone, so RP-17 will keep failing and there is no key left to cut
			// off. Drop the marker instead of failing every run forever.
			if clearErr := markers.Clear(withTenantGUC(ctx, p.TenantID), p.TenantID, p.RequestedAt); clearErr != nil {
				result.Failed++
				logError(ctx, log, "keys refresh retry: dropping the marker of an offboarded tenant failed", map[string]interface{}{"tenant_id": p.TenantID}, clearErr)
				continue
			}
			result.Dropped++
			if log != nil {
				log.Warn("keys refresh retry: RP-17 failed for a tenant with no principal left — marker dropped", fieldsWithTrace(ctx, map[string]interface{}{"tenant_id": p.TenantID, "error": err.Error()}))
			}
		default:
			result.Failed++
			logError(ctx, log, "keys refresh retry: RP-17 key-cache refresh still failing — marker kept for the next run", map[string]interface{}{
				"tenant_id": p.TenantID, "requested_at": p.RequestedAt,
			}, err)
		}
	}
	return result
}

// refreshAndClear calls RP-17 for tenantID and, only if it succeeded,
// clears the tenant's marker up to upTo (the marker value observed before
// the call — a request written since is left for its own refresh). A
// failed clear is logged, not returned: the refresh itself landed, and the
// leftover marker only costs one harmless extra RP-17 next run.
func refreshAndClear(ctx context.Context, markers keysRefreshMarkers, rp port.RealmProvisionerClient, tenantID uuid.UUID, upTo time.Time, log port.Logger) error {
	if err := refreshKeysDetached(ctx, rp, tenantID); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(withTenantGUC(ctx, tenantID)), rp17Timeout)
	defer cancel()
	if err := markers.Clear(cctx, tenantID, upTo); err != nil && log != nil {
		log.Warn("keys refresh: RP-17 succeeded but clearing its marker failed — the next run repeats the (harmless) refresh", fieldsWithTrace(ctx, map[string]interface{}{"tenant_id": tenantID, "error": err.Error()}))
	}
	return nil
}

// refreshKeysDetached calls RP-17 on a context that keeps ctx's values
// (trace, logger fields) but not its deadline or cancellation (SIGTERM),
// bounded by rp17Timeout: once a rotation has committed, cutting the
// cache-clear off mid-call would manufacture the two-halves gap.
func refreshKeysDetached(ctx context.Context, rp port.RealmProvisionerClient, tenantID uuid.UUID) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rp17Timeout)
	defer cancel()
	return rp.RefreshKeys(rctx, tenantID)
}

// tenantBatch is one tenant's share of an enumerated batch.
type tenantBatch[T any] struct {
	TenantID uuid.UUID
	Rows     []T
}

// groupByTenant groups rows by tenant in order of each tenant's first
// appearance, so the repository's ORDER BY (oldest expires_at /
// next_rotation_at first) still decides which tenants go first.
func groupByTenant[T any](rows []T, tenantOf func(T) uuid.UUID) []tenantBatch[T] {
	index := map[uuid.UUID]int{}
	var out []tenantBatch[T]
	for _, row := range rows {
		id := tenantOf(row)
		i, seen := index[id]
		if !seen {
			i = len(out)
			index[id] = i
			out = append(out, tenantBatch[T]{TenantID: id})
		}
		out[i].Rows = append(out[i].Rows, row)
	}
	return out
}

// countRows is the number of rows across batches.
func countRows[T any](batches []tenantBatch[T]) int {
	n := 0
	for _, b := range batches {
		n += len(b.Rows)
	}
	return n
}

// stopStarting reports whether no new unit of work needing `need` may
// start: the run was signalled (SIGTERM/SIGINT cancels ctx) or fewer than
// need remain before its deadline.
func stopStarting(ctx context.Context, need time.Duration) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < need
}

// workCtx returns the context one admitted unit of work (a tenant, a
// principal) runs on: ctx's values and deadline, but not its signal
// cancellation — a SIGTERM stops new work from starting (stopStarting) but
// must not abort the in-flight unit between its OpenBao call and its
// commit.
func workCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(detached, deadline)
	}
	return context.WithCancel(detached)
}
