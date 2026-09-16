package main

import (
	"context"
	"errors"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// sweepResult summarizes one overlap-expiry sweep run (§8.3). Returned
// (rather than recorded directly to a metrics port) because reconciler_jobs
// may depend only on domain/port/postgres/openbao (.go-arch-lint.yml) —
// metric instrumentation around these counts is wired in main.go / a later
// milestone.
type sweepResult struct {
	Revoked int
	Skipped int // already handled — a race with an opportunistic TS-1 sweep or a prior rotator run
	Failed  int
}

// errAlreadyHandled signals that an enumerated row was no longer
// `rotating` by the time this job reached it — not a failure.
var errAlreadyHandled = errors.New("rotator: credential already revoked")

// runOverlapSweep implements §8.3: enumerate every `rotating` credential
// across all tenants whose expires_at has passed (via the BYPASSRLS
// reconciler role), then revoke each one through a normal RLS-scoped
// RunInTx bound to that row's own tenant (RLS-7). Availability-first: one
// row's failure is logged and does not abort the rest of the sweep — a
// missed row is caught by the next run.
func runOverlapSweep(ctx context.Context, reconciler port.ReconcilerRepository, credentials port.CredentialRepository, secrets port.SecretStore, tx port.TxRunner, log port.Logger) sweepResult {
	var result sweepResult
	expired, err := reconciler.ListExpiredRotating(ctx)
	if err != nil {
		logError(ctx, log, "overlap sweep: list expired rotating failed", nil, err)
		return result
	}
	for _, row := range expired {
		err := revokeExpiredRotating(ctx, credentials, secrets, tx, row)
		switch {
		case err == nil:
			result.Revoked++
		case errors.Is(err, errAlreadyHandled):
			result.Skipped++
		default:
			result.Failed++
			logError(ctx, log, "overlap sweep: revoke failed", map[string]interface{}{
				"tenant_id": row.TenantID, "principal_id": row.PrincipalID, "version": row.Version,
			}, err)
		}
	}
	return result
}

// revokeExpiredRotating revokes one expired rotating credential: deletes
// its OpenBao material, then flips status=revoked and emits
// ServiceAccountCredentialRevoked in one RLS-scoped transaction bound to
// the row's own tenant. Mirrors CredentialService's revoke discipline
// (material-first, §9.3) directly against domain/port rather than
// core/service — reconciler_jobs may not depend on service
// (.go-arch-lint.yml).
func revokeExpiredRotating(ctx context.Context, credentials port.CredentialRepository, secrets port.SecretStore, tx port.TxRunner, row port.ExpiredRotatingCredential) error {
	ctx = withTenantGUC(ctx, row.TenantID)

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
	return tx.RunInTx(ctx, func(ctx context.Context) error {
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
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventServiceAccountCredentialRevoked, TenantID: row.TenantID, Actor: domain.SystemPrincipalID,
			Data: domain.ServiceAccountCredentialRevokedPayload{
				TenantID: row.TenantID, PrincipalID: row.PrincipalID, Version: row.Version, RevokedAt: now.Format(time.RFC3339),
			},
		})
	})
}
