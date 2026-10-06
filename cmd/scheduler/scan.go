package main

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// credentialIssuer is the subset of *service.CredentialService (or its
// metrics-instrumented decorator, main.go) this job calls — mirrors
// httpadapter.CredentialHandler's identical local-interface pattern
// (internal/adapter/inbound/http/credential_handler.go) for the same
// reason: cmd may depend on both service and observability, but keeping
// this file's signature in terms of the narrow interface means a fake in
// scan_test.go never needs to satisfy Revoke too.
type credentialIssuer interface {
	IssueOrRotate(ctx context.Context, tenantID, principalID uuid.UUID, req service.IssueOrRotateRequest, actor uuid.UUID) (*service.IssueOrRotateResult, error)
}

// cadenceScanResult summarizes one due-list scan run (§16 TSQ-6 Resolved).
type cadenceScanResult struct {
	Rotated  int
	Skipped  int // a concurrent operator/O&M rotation, or an offboarding revoke, already handled this principal
	Failed   int // IssueOrRotate failed, OR it succeeded but the RP-17 key-refresh failed (see rotateOneDue), OR the enumeration failed
	Deferred int // not started: out of run budget, or the run was signalled — the next run picks them up
}

// rowReserve is the minimum run time left before starting another
// rotation (RSA keygen + OpenBao write + commit): a rotation must never be
// cut off by the run deadline after it has committed.
const rowReserve = 20 * time.Second

// rp17Timeout bounds the post-commit RP-17 call, which runs detached from
// the run deadline so a committed rotation's cache-clear is not cut off
// mid-call (that would manufacture the two-halves gap).
const rp17Timeout = 10 * time.Second

// tenantReserve is the run time a tenant needs before it may start (and
// before each further row of a started tenant): one rotation plus the
// tenant's inline RP-17 call — so RP-17 always fits inside the run budget
// rather than in an unguarded flush after the loop.
const tenantReserve = rowReserve + rp17Timeout

// errAlreadyHandled signals that a due row was no longer eligible by the
// time this job reached it — not a failure.
var errAlreadyHandled = errors.New("scheduler: credential already rotated or principal no longer eligible")

// runCadenceScan implements §16 TSQ-6 Resolved / TS-D14: enumerate every
// `active` credential across all tenants whose next_rotation_at has
// passed (via the BYPASSRLS reconciler role), then, tenant by tenant,
// rotate each one through the ordinary RLS-scoped
// CredentialService.IssueOrRotate call bound to that row's own tenant
// (RLS-7) — exactly the write path an HTTP-triggered TS-1 call takes — and
// call RP-17 for the tenant inline once its rotations have committed.
// Rows are grouped by tenant in the repository's most-overdue-first order;
// a tenant starts only with tenantReserve left and not after SIGTERM, and
// tenants not started are Deferred. Before each IssueOrRotate the tenant's
// keys_refresh_pending intent marker is written (intent-first), and after it
// the marker is re-written as committed, so a rotation whose RP-17 fails or
// never runs is retried by the next run instead of being lost once
// next_rotation_at has advanced. Availability-first: one
// principal's failure is logged and does not abort the rest of the scan.
// overlapSeconds is the overlap each rotation keeps the outgoing key valid
// for (ROTATION_DEFAULT_OVERLAP_SECONDS, the same default TS-1 applies
// when a caller omits it).
func runCadenceScan(ctx context.Context, reconciler port.ReconcilerRepository, credentials credentialIssuer, markers keysRefreshMarkers, rp port.RealmProvisionerClient, overlapSeconds, limit int, log port.Logger) cadenceScanResult {
	var result cadenceScanResult
	due, err := reconciler.ListDueForRotation(ctx, limit)
	if err != nil {
		// Not a safe no-op: if enumeration fails every run, cadence rotation
		// silently stops. Fail the run so the Job (and its alert) shows it.
		result.Failed++
		logError(ctx, log, "cadence scan: list due for rotation failed", nil, err)
		return result
	}

	batches := groupByTenant(due, func(r port.DueForRotation) uuid.UUID { return r.TenantID })
	for i, batch := range batches {
		if stopStarting(ctx, tenantReserve) {
			result.Deferred += countRows(batches[i:])
			if log != nil {
				log.Warn("cadence scan: run budget nearly spent or run signalled — deferring the remaining tenants to the next run", fieldsWithTrace(ctx, map[string]interface{}{
					"deferred_rows": countRows(batches[i:]), "deferred_tenants": len(batches) - i,
				}))
			}
			break
		}
		scanTenant(ctx, credentials, markers, rp, overlapSeconds, batch, &result, log)
	}
	return result
}

// scanTenant rotates one tenant's due rows and then calls RP-17 for it, on
// workCtx: a SIGTERM stops further rows (Deferred) but never cuts off the
// in-flight rotation or the RP-17 for what committed.
func scanTenant(ctx context.Context, credentials credentialIssuer, markers keysRefreshMarkers, rp port.RealmProvisionerClient, overlapSeconds int, batch tenantBatch[port.DueForRotation], result *cadenceScanResult, log port.Logger) {
	wctx, cancel := workCtx(ctx)
	defer cancel()

	var (
		rotated   []port.DueForRotation
		upTo      time.Time // newest marker value written for this tenant
		fresh     bool      // the first marker write found no earlier request owed
		ambiguous bool      // a failure that may still have committed — the marker must stay
	)
	for j, row := range batch.Rows {
		if j > 0 && stopStarting(ctx, tenantReserve) {
			result.Deferred += len(batch.Rows) - j
			break
		}
		// Intent first: the marker commits before the rotation can, so a
		// crash anywhere after IssueOrRotate's commit still leaves a refresh
		// owed on record. It is an INTENT marker: the retry pass leaves it
		// alone until intent_until, so no other run refreshes (and clears)
		// it before this rotation has committed.
		mark, err := markers.MarkIntent(withTenantGUC(wctx, row.TenantID), row.TenantID, intentTTLFor(ctx))
		if err != nil {
			result.Failed++
			logError(ctx, log, "cadence scan: writing the keys-refresh marker failed — rotation not attempted", map[string]interface{}{
				"tenant_id": row.TenantID, "principal_id": row.PrincipalID, "version": row.Version,
			}, err)
			continue
		}
		if upTo.IsZero() {
			fresh = mark.Fresh
		}
		upTo = later(upTo, mark.RequestedAt)

		err = rotateOneDue(wctx, credentials, row, overlapSeconds)
		if !errors.Is(err, errAlreadyHandled) {
			// The rotation committed, or may have. Re-mark — now as a
			// COMMITTED marker — and clear only up to this newer value: a
			// concurrent rotator sweep for the same tenant may have marked,
			// refreshed and cleared the row (its Clear(<=its own mark) also
			// covers our older intent) between our intent write and our
			// commit, and that refresh ran before our new key existed.
			// Without this, our RP-17 failing next would leave no marker
			// while next_rotation_at has already advanced — never retried.
			upTo = later(upTo, remarkCommitted(ctx, wctx, markers, row, log))
		}
		switch {
		case err == nil:
			rotated = append(rotated, row)
		case errors.Is(err, errAlreadyHandled):
			result.Skipped++
		default:
			ambiguous = true
			result.Failed++
			logError(ctx, log, "cadence scan: rotate failed", map[string]interface{}{
				"tenant_id": row.TenantID, "principal_id": row.PrincipalID, "version": row.Version,
			}, err)
		}
	}

	if len(rotated) == 0 {
		// Nothing committed. Withdraw the intent marker only when it is
		// provably ours alone: it was fresh (no earlier refresh was owed) and
		// every attempt was refused outright. Otherwise leave it — the next
		// run's RP-17 is at worst a harmless repeat.
		if fresh && !ambiguous && !upTo.IsZero() {
			cctx, ccancel := context.WithTimeout(context.WithoutCancel(withTenantGUC(wctx, batch.TenantID)), rp17Timeout)
			if err := markers.Clear(cctx, batch.TenantID, upTo); err != nil && log != nil {
				log.Warn("cadence scan: withdrawing an unused keys-refresh marker failed — the next run repeats a harmless RP-17", fieldsWithTrace(ctx, map[string]interface{}{"tenant_id": batch.TenantID, "error": err.Error()}))
			}
			ccancel()
		}
		return
	}

	if err := refreshAndClear(wctx, markers, rp, batch.TenantID, upTo, log); err != nil {
		// The documented two-halves gap (rotateOneDue's doc comment): the
		// rotations are committed and next_rotation_at has advanced. Still
		// page-worthy, but a committed marker must stay so the next run
		// retries it. Re-mark rather than trust the one written above: a
		// concurrent run's retry pass or sweep may have cleared it since
		// (with an RP-17 that, too, may have preceded nothing of ours).
		remarkCommitted(ctx, wctx, markers, batch.Rows[0], log)
		result.Failed += len(rotated)
		for _, row := range rotated {
			logError(ctx, log, "cadence scan: rotated but RP-17 key-cache refresh failed — marker kept, the next run retries", map[string]interface{}{
				"tenant_id": row.TenantID, "principal_id": row.PrincipalID, "version": row.Version,
			}, err)
		}
		return
	}
	result.Rotated += len(rotated)
}

// intentTTL is how long cmd/scheduler's intent marker keeps the retry pass
// away (keys_refresh_pending.intent_until). It must exceed the longest its
// writer can sit between MarkIntent and the committed re-mark that follows
// IssueOrRotate — one IssueOrRotate attempt, itself bounded by the run
// deadline — so that an intent only ever expires once its writer has
// finished or died; an expired intent is then retried like any committed
// marker (never lost). 4 minutes is comfortably above the default
// SCHEDULER_RUN_TIMEOUT (180s) plus rp17Timeout; intentTTLFor stretches it
// when an operator configures a longer run.
const intentTTL = 4 * time.Minute

// intentTTLFor is intentTTL, or the run's remaining time plus rp17Timeout
// and a 30s margin if that is longer (the default 180s run stays on the
// floor).
func intentTTLFor(ctx context.Context) time.Duration {
	ttl := intentTTL
	if deadline, ok := ctx.Deadline(); ok {
		if d := time.Until(deadline) + rp17Timeout + 30*time.Second; d > ttl {
			ttl = d
		}
	}
	return ttl
}

// remarkCommitted writes row's tenant a COMMITTED marker (MarkPending) on a
// context detached from SIGTERM and returns its value, or the zero time if
// the write failed. A failure is logged, not fatal: the intent marker
// written before IssueOrRotate is still there (unless a concurrent run
// cleared it, the narrow residual this re-mark exists to close) and becomes
// retryable once its intent_until passes.
func remarkCommitted(ctx, wctx context.Context, markers keysRefreshMarkers, row port.DueForRotation, log port.Logger) time.Time {
	mctx, cancel := context.WithTimeout(context.WithoutCancel(withTenantGUC(wctx, row.TenantID)), rp17Timeout)
	defer cancel()
	mark, err := markers.MarkPending(mctx, row.TenantID)
	if err != nil {
		if log != nil {
			log.Warn("cadence scan: re-marking the keys refresh as committed failed — the intent marker is retried once it expires", fieldsWithTrace(ctx, map[string]interface{}{
				"tenant_id": row.TenantID, "principal_id": row.PrincipalID, "error": err.Error(),
			}))
		}
		return time.Time{}
	}
	return mark.RequestedAt
}

// later returns the later of a and b.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// rotateOneDue rotates one due principal: calls IssueOrRotate under a
// fresh rotation_id (§9.2 idempotency still protects a same-run retry
// this function does not itself perform). The caller (scanTenant) then
// asks the Realm Provisioner to refresh Keycloak's cached JWKS for the
// tenant realm (RP-17) so the newly-issued key is recognized. No key
// material is passed to RP — under the EXT-6 client-jwt/JWKS mechanism
// (rev 1.3) Keycloak fetches the public key from this service's own JWKS
// itself; RP only triggers the cache-clear (TS-INV-1: this service never
// writes Keycloak).
//
// A known gap, inherent to automating a two-service handshake without a
// distributed transaction (the same two-halves reality TS-INV-7 already
// documents for revoke, §8.7): if IssueOrRotate commits but the RP-17
// key-refresh then fails, this principal's next_rotation_at has already
// advanced to the new cadence window, so it will NOT reappear on the next
// scan. The keys_refresh_pending marker scanTenant writes before calling
// this function is what closes the gap: it survives the failure (or a
// killed Pod) and the next run's retryPendingKeyRefreshes repeats the
// RP-17 call. The failure is still surfaced as Failed (page-worthy,
// §11.5) with the tenant/principal/version logged (never key material,
// TS-INV-2) — never by re-rotating.
func rotateOneDue(ctx context.Context, credentials credentialIssuer, row port.DueForRotation, overlapSeconds int) error {
	ctx = withTenantGUC(ctx, row.TenantID)

	if _, err := credentials.IssueOrRotate(ctx, row.TenantID, row.PrincipalID, service.IssueOrRotateRequest{
		RotationID:     uuid.New(),
		OverlapSeconds: overlapSeconds,
		// Rotate only the version this scan found due: if an operator
		// rotated the principal since enumeration, the service refuses
		// (optimistic_lock_conflict) instead of rotating the fresh key again.
		ExpectActiveVersion: row.Version,
	}, domain.SystemPrincipalID); err != nil {
		var de *domain.Error
		if errors.As(err, &de) {
			switch de.Code {
			case domain.ErrPrincipalRevoked, domain.ErrPrincipalNotFound:
				// Offboarding or a break-glass revoke won the race since
				// this row was enumerated — nothing left to rotate. An
				// offboarding cascade that has already soft-deleted the
				// principal surfaces as principal_not_found rather than
				// principal_revoked; neither is a failure worth paging on.
				return errAlreadyHandled
			case domain.ErrRotationInFlight, domain.ErrOptimisticLockConflict:
				// A concurrent operator/O&M rotation is in flight or already
				// committed (the active version moved since enumeration); its
				// own IssueOrRotate call advances next_rotation_at.
				return errAlreadyHandled
			}
		}
		return err
	}
	return nil
}
