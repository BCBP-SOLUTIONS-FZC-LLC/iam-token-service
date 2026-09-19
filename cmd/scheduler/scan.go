package main

import (
	"context"
	"errors"

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
	Rotated int
	Skipped int // a concurrent operator/O&M rotation, or an offboarding revoke, already handled this principal
	Failed  int // IssueOrRotate itself failed, OR it succeeded but the RP-17 key-refresh failed (see rotateOneDue)
}

// errAlreadyHandled signals that a due row was no longer eligible by the
// time this job reached it — not a failure.
var errAlreadyHandled = errors.New("scheduler: credential already rotated or principal no longer eligible")

// runCadenceScan implements §16 TSQ-6 Resolved / TS-D14: enumerate every
// `active` credential across all tenants whose next_rotation_at has
// passed (via the BYPASSRLS reconciler role), then rotate each one
// through the ordinary RLS-scoped CredentialService.IssueOrRotate call
// bound to that row's own tenant (RLS-7) — exactly the write path an
// HTTP-triggered TS-1 call takes. Availability-first: one principal's
// failure is logged and does not abort the rest of the scan — a missed
// principal is caught by the next run (subject to the caveat in
// rotateOneDue's doc comment).
func runCadenceScan(ctx context.Context, reconciler port.ReconcilerRepository, credentials credentialIssuer, rp port.RealmProvisionerClient, log port.Logger) cadenceScanResult {
	var result cadenceScanResult
	due, err := reconciler.ListDueForRotation(ctx)
	if err != nil {
		logError(ctx, log, "cadence scan: list due for rotation failed", nil, err)
		return result
	}
	for _, row := range due {
		err := rotateOneDue(ctx, credentials, rp, row)
		switch {
		case err == nil:
			result.Rotated++
		case errors.Is(err, errAlreadyHandled):
			result.Skipped++
		default:
			result.Failed++
			logError(ctx, log, "cadence scan: rotate failed", map[string]interface{}{
				"tenant_id": row.TenantID, "principal_id": row.PrincipalID, "version": row.Version,
			}, err)
		}
	}
	return result
}

// rotateOneDue rotates one due principal: calls IssueOrRotate under a
// fresh rotation_id (§9.2 idempotency still protects a same-run retry
// this function does not itself perform), then asks the Realm Provisioner
// to refresh Keycloak's cached JWKS for the tenant realm (RP-17) so the
// newly-issued key is recognized. No key material is passed to RP — under
// the EXT-6 client-jwt/JWKS mechanism (rev 1.3) Keycloak fetches the
// public key from this service's own JWKS itself; RP only triggers the
// cache-clear (TS-INV-1: this service never writes Keycloak).
//
// A known gap, inherent to automating a two-service handshake without a
// distributed transaction (the same two-halves reality TS-INV-7 already
// documents for revoke, §8.7): if IssueOrRotate commits but the RP-17
// key-refresh then fails, this principal's next_rotation_at has already
// advanced to the new cadence window, so it will NOT reappear on the next
// scan — this service's own record and Keycloak's cached keys are now out
// of sync until someone acts. That failure is deliberately surfaced as a
// Failed outcome (page-worthy, §11.5) with the tenant/principal/version
// logged (never key material, TS-INV-2) so an operator can trigger the
// missed RP-17 refresh by hand, the same as any other break-glass path
// (§8.7) — not silently retried into a possible double-rotation.
func rotateOneDue(ctx context.Context, credentials credentialIssuer, rp port.RealmProvisionerClient, row port.DueForRotation) error {
	ctx = withTenantGUC(ctx, row.TenantID)

	if _, err := credentials.IssueOrRotate(ctx, row.TenantID, row.PrincipalID, service.IssueOrRotateRequest{
		RotationID:     uuid.New(),
		OverlapSeconds: domain.DefaultOverlapSeconds,
	}, domain.SystemPrincipalID); err != nil {
		var de *domain.Error
		if errors.As(err, &de) {
			switch de.Code {
			case domain.ErrPrincipalRevoked:
				// Offboarding or a break-glass revoke won the race since
				// this row was enumerated — nothing left to rotate.
				return errAlreadyHandled
			case domain.ErrRotationInFlight:
				// A concurrent operator/O&M-triggered rotation is already
				// in flight for this principal; its own IssueOrRotate call
				// already advances next_rotation_at.
				return errAlreadyHandled
			}
		}
		return err
	}

	if err := rp.RefreshKeys(ctx, row.TenantID); err != nil {
		return err
	}
	return nil
}
