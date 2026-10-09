package main

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// sweepResult summarizes one overlap-expiry sweep run (§8.3). Returned
// (rather than recorded directly to a metrics port) because reconciler_jobs
// may depend only on domain/port/postgres/openbao (.go-arch-lint.yml);
// main.go records it (recordSweepMetric) and turns Failed into the exit code.
type sweepResult struct {
	Revoked  int
	Skipped  int // already handled — a race with an opportunistic TS-1 sweep or a prior rotator run
	Failed   int // a revoke failed, its RP-17 refresh failed, or the enumeration itself failed
	Deferred int // not started: out of run budget, or the run was signalled — the next run picks them up
}

// rowReserve is the minimum run time left before starting another row: a
// revoke (OpenBao delete + commit) must never be cut off between its delete
// and its commit by the run deadline, which would leave a `rotating` row
// with no material (a JWKS key error) until the next run.
const rowReserve = 20 * time.Second

// rp17Timeout bounds the post-commit RP-17 call, which runs on a context
// detached from the run deadline: once a revoke has committed, cutting the
// cache-clear off mid-call would manufacture the two-halves gap.
const rp17Timeout = 10 * time.Second

// tenantReserve is the run time a tenant needs before it may start (and
// before each further row of a started tenant): one row plus the tenant's
// RP-17 call. The tenant's RP-17 runs inline right after its rows, inside
// the run budget, so a large batch can no longer overrun
// activeDeadlineSeconds in an unguarded post-loop RP-17 flush.
const tenantReserve = rowReserve + rp17Timeout

// errAlreadyHandled signals that an enumerated row was no longer
// `rotating` by the time this job reached it — not a failure.
var errAlreadyHandled = errors.New("rotator: credential already revoked")

// runOverlapSweep implements §8.3: enumerate every `rotating` credential
// across all tenants whose expires_at has passed (via the BYPASSRLS
// reconciler role), then, tenant by tenant, revoke each one through a
// normal RLS-scoped RunInTx bound to that row's own tenant (RLS-7) and ask
// the Realm Provisioner to clear Keycloak's cached JWKS for that tenant
// (RP-17, `ClearServiceAccountKeysCache`) — under the EXT-6
// client-jwt/JWKS mechanism (rev 1.3) a cached key keeps authenticating at
// Keycloak indefinitely until that call lands; there is no Keycloak-side
// TTL that does it automatically (TS-INV-7, §6.2).
//
// Rows are grouped by tenant (in the repository's oldest-first order) and
// each tenant's RP-17 runs inline after its rows, so it is inside the run
// budget: a tenant starts only with tenantReserve left, and tenants not
// started — out of budget, or SIGTERM — are Deferred, never half-done.
// Each revoke also writes the tenant's keys_refresh_pending marker in its
// own transaction, so a refresh that fails or never runs (the Pod killed
// after the commit) is retried by the next run's retryPendingKeyRefreshes.
// Availability-first: one row's failure is logged and does not abort the
// rest of the sweep.
func runOverlapSweep(ctx context.Context, reconciler port.ReconcilerRepository, credentials port.CredentialRepository, secrets port.SecretStore, tx port.TxRunner, markers keysRefreshMarkers, rp port.RealmProvisionerClient, limit int, log port.Logger) sweepResult {
	var result sweepResult
	expired, err := reconciler.ListExpiredRotating(ctx, limit)
	if err != nil {
		// Not a safe no-op: if enumeration fails every run (reconciler role,
		// DSN, Postgres down), expired keys silently stay valid. Fail the run.
		result.Failed++
		logError(ctx, log, "overlap sweep: list expired rotating failed", nil, err)
		return result
	}

	batches := groupByTenant(expired, func(r port.ExpiredRotatingCredential) uuid.UUID { return r.TenantID })
	for i, batch := range batches {
		if stopStarting(ctx, tenantReserve) {
			result.Deferred += countRows(batches[i:])
			if log != nil {
				log.Warn("overlap sweep: run budget nearly spent or run signalled — deferring the remaining tenants to the next run", fieldsWithTrace(ctx, map[string]interface{}{
					"deferred_rows": countRows(batches[i:]), "deferred_tenants": len(batches) - i,
				}))
			}
			break
		}
		sweepTenant(ctx, credentials, secrets, tx, markers, rp, batch, &result, log)
	}
	return result
}

// sweepTenant revokes one tenant's expired rows and then calls RP-17 for
// it. The tenant runs on workCtx: a SIGTERM stops further rows (Deferred)
// but never cuts the in-flight revoke or the RP-17 for what committed.
func sweepTenant(ctx context.Context, credentials port.CredentialRepository, secrets port.SecretStore, tx port.TxRunner, markers keysRefreshMarkers, rp port.RealmProvisionerClient, batch tenantBatch[port.ExpiredRotatingCredential], result *sweepResult, log port.Logger) {
	wctx, cancel := workCtx(ctx)
	defer cancel()

	var (
		revoked []port.ExpiredRotatingCredential
		upTo    time.Time // newest marker value this tenant's revokes wrote
	)
	for j, row := range batch.Rows {
		if j > 0 && stopStarting(ctx, tenantReserve) {
			result.Deferred += len(batch.Rows) - j
			break
		}
		mark, err := revokeExpiredRotating(wctx, credentials, secrets, tx, markers, row)
		switch {
		case err == nil:
			revoked = append(revoked, row)
			if mark.After(upTo) {
				upTo = mark
			}
		case errors.Is(err, errAlreadyHandled):
			result.Skipped++
		default:
			result.Failed++
			logError(ctx, log, "overlap sweep: revoke failed", map[string]interface{}{
				"tenant_id": row.TenantID, "principal_id": row.PrincipalID, "version": row.Version,
			}, err)
		}
	}
	if len(revoked) == 0 {
		return
	}

	// The DB half of every revoke above is durably committed together with
	// the tenant's marker. A failed RP-17 is still Failed (page-worthy) —
	// a revoked key keeps authenticating until it lands — but no longer a
	// silent, permanent loss: the marker stays and the next run retries.
	if err := refreshAndClear(wctx, markers, rp, batch.TenantID, upTo, log); err != nil {
		result.Failed += len(revoked)
		for _, row := range revoked {
			logError(ctx, log, "overlap sweep: revoked but RP-17 key-cache refresh failed — marker kept, the next run retries", map[string]interface{}{
				"tenant_id": row.TenantID, "principal_id": row.PrincipalID, "version": row.Version,
			}, err)
		}
		return
	}
	result.Revoked += len(revoked)
}

// revokeExpiredRotating revokes one expired rotating credential: deletes
// its OpenBao material, then flips status=revoked, emits
// ServiceAccountCredentialRevoked and upserts the tenant's
// keys_refresh_pending marker, all in one RLS-scoped transaction bound to
// the row's own tenant. Returns the marker value written. Mirrors
// CredentialService's revoke discipline (material-first, §9.3) directly
// against domain/port rather than core/service — reconciler_jobs may not
// depend on service (.go-arch-lint.yml).
//
// The row is (re-)read INSIDE the closure: RunInTx retries the closure on a
// serialization failure or deadlock, and Update scans the bumped
// record_version back into the struct, so a retry must start from a fresh
// read, never from a struct a failed attempt mutated (CredentialService
// does the same).
//
// The caller (sweepTenant) calls RP-17 once per tenant after these commit;
// the marker written here is what makes a failed or never-made RP-17 call
// recoverable — the row itself is `revoked` and ListExpiredRotating will
// never enumerate it again.
func revokeExpiredRotating(ctx context.Context, credentials port.CredentialRepository, secrets port.SecretStore, tx port.TxRunner, markers keysRefreshMarkers, row port.ExpiredRotatingCredential) (time.Time, error) {
	ctx = withTenantGUC(ctx, row.TenantID)

	var marked time.Time
	err := tx.RunInTx(ctx, func(ctx context.Context) error {
		marked = time.Time{}
		cred, err := credentials.FindByVersion(ctx, row.TenantID, row.PrincipalID, row.Version)
		if err != nil {
			return err
		}
		if cred == nil || cred.Status != domain.CredentialStatusRotating {
			return errAlreadyHandled
		}
		if err := secrets.Delete(ctx, cred.OpenBaoPath); err != nil {
			return err
		}

		now := time.Now().UTC()
		cred.Status = domain.CredentialStatusRevoked
		cred.RevokedAt = &now
		if err := credentials.Update(ctx, cred); err != nil {
			// A concurrent actor (a TS-2 call racing this sweep, or another
			// rotator run) may have revoked this exact row between the
			// FindByVersion read above and this write — record_version no
			// longer matches. That is the same benign race errAlreadyHandled
			// exists for, just detected one step later (at the write instead
			// of the initial read); classify it as Skipped, not Failed, so
			// rotation_sweep_total{result="failed"} doesn't page on-call for
			// a race that already converged correctly.
			var de *domain.Error
			if errors.As(err, &de) && de.Code == domain.ErrOptimisticLockConflict {
				return errAlreadyHandled
			}
			return err
		}
		mark, err := markers.MarkPending(ctx, row.TenantID)
		if err != nil {
			return err
		}
		marked = mark.RequestedAt
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventServiceAccountCredentialRevoked, TenantID: row.TenantID, Actor: domain.SystemPrincipalID,
			Data: domain.ServiceAccountCredentialRevokedPayload{
				TenantID: row.TenantID, PrincipalID: row.PrincipalID, Version: row.Version,
				ActorID: domain.SystemPrincipalID, RevokedAt: now.Format(time.RFC3339),
			},
		})
	})
	if err != nil {
		return time.Time{}, err
	}
	return marked, nil
}
