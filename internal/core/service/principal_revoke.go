package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// RevokePrincipalResult is the TS-7 response body (§5.4). Every field is read
// back from the committed rows, so an idempotent repeat against an
// already-revoked principal returns the same body as the call that revoked it.
type RevokePrincipalResult struct {
	PrincipalID      uuid.UUID
	TenantID         uuid.UUID
	KeycloakClientID string
	Status           domain.PrincipalStatus
	RecordVersion    int
	// RevokedAt is the principal row's updated_at: the revoke is the last
	// write a principal ever gets (TS-1 refuses a revoked principal, 422).
	RevokedAt time.Time
	// Versions lists every credential version of the principal, ascending —
	// all `revoked` once TS-7 has run.
	Versions []int
	// NewlyRevoked counts the versions this call moved to `revoked`; 0 on an
	// idempotent repeat. Not on the wire (the metrics decorator reads it).
	NewlyRevoked int
}

// ServiceAccountRevokedEvent builds the principal-level ServiceAccountRevoked
// event (§7.5) — shared by the offboarding cascade and TS-7, so both emit the
// same payload.
func ServiceAccountRevokedEvent(tenantID, principalID, actor uuid.UUID, revokedAt time.Time) *domain.Event {
	return &domain.Event{
		Type: domain.EventServiceAccountRevoked, TenantID: tenantID, Actor: actor,
		Data: domain.ServiceAccountRevokedPayload{
			TenantID: tenantID, PrincipalID: principalID,
			ActorID: actor, RevokedAt: revokedAt.UTC().Format(time.RFC3339),
		},
	}
}

// revokePrincipal implements TS-7 (§5.4): retire the tenant's
// platform-automation principal for good, without the hard delete that only
// the TenantMembershipsPurged cascade performs. The Realm Provisioner's
// orphaned-signup-reap calls it for a trial signup that never reached O&M,
// so no purge event will ever arrive for it.
//
// One transaction, under the principal row lock TS-1/TS-2 take (so an
// in-flight rotation is waited for, or reported as 409 rotation_in_flight
// after PG_LOCK_TIMEOUT):
//
//  1. re-read the principal under the lock; already `revoked` → idempotent;
//  2. erase the OpenBao material — every row's path plus the principal's
//     client subtree — through EraseSecretMaterial, the offboarding
//     cascade's own step, each call bounded like TS-1/TS-2's in-lock write;
//  3. move every live version to `revoked` (cadence cleared, so cmd/scheduler
//     never rotates it again) through markCredentialRevoked, TS-2's own step,
//     each emitting ServiceAccountCredentialRevoked;
//  4. set the principal `revoked` (record_version-guarded) and emit
//     ServiceAccountRevoked, the event the offboarding cascade emits.
//
// Material goes first (CUST-2): an OpenBao failure rolls the transaction
// back with nothing committed; material already deleted by then is gone, the
// rows still say live, and a retry converges because Delete is a no-op on a
// missing path. Like TS-2 this is this service's half only (TS-INV-7): the
// caller removes or invalidates the Keycloak client.
func (s *CredentialService) revokePrincipal(ctx context.Context, tenantID, actor uuid.UUID) (*RevokePrincipalResult, error) {
	p, err := s.principals.FindByType(ctx, tenantID, domain.PrincipalTypePlatformAutomation)
	if err != nil {
		return nil, err
	}
	if p.IsRevoked() {
		return s.revokedPrincipalResult(ctx, p, 0)
	}

	var (
		revoked      *domain.ServiceAccountPrincipal
		newlyRevoked int
		// txCreds holds the credential rows read inside the transaction so the
		// result builder never re-reads them outside it (M-19). It is set by
		// the closure below, populated from the ListByPrincipal already
		// performed there, and consumed by buildRevokedResult after the
		// transaction commits.
		txCreds []*domain.Credential
	)
	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		revoked, newlyRevoked = nil, 0
		txCreds = nil
		locked, err := s.principals.LockForUpdate(ctx, tenantID, p.ID)
		if err != nil {
			return err
		}
		if locked.IsRevoked() {
			// A concurrent TS-7 won while this one waited for the lock.
			// Read credentials here, inside the tx, so the race path also
			// avoids a second ListByPrincipal outside the transaction (M-19).
			creds, err := s.credentials.ListByPrincipal(ctx, tenantID, locked.ID)
			if err != nil {
				return err
			}
			txCreds = creds
			revoked = locked
			return nil
		}

		creds, err := s.credentials.ListByPrincipal(ctx, tenantID, locked.ID)
		if err != nil {
			return err
		}
		txCreds = creds
		paths := make([]string, 0, len(creds))
		for _, c := range creds {
			paths = append(paths, c.OpenBaoPath)
		}
		subtree := domain.OpenBaoPathPrefixFor(tenantID, locked.KeycloakClientID)
		if err := EraseSecretMaterial(ctx, s.secrets, paths, subtree, inLockSecretWriteTimeout); err != nil {
			return err
		}

		now := time.Now().UTC()
		for _, c := range creds {
			if c.Status == domain.CredentialStatusRevoked {
				continue
			}
			if err := s.markCredentialRevoked(ctx, tenantID, locked.ID, c, actor, now); err != nil {
				return err
			}
			newlyRevoked++
		}

		locked.Status = domain.PrincipalStatusRevoked
		if err := s.principals.UpdateStatus(ctx, locked); err != nil {
			return err
		}
		revoked = locked

		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, ServiceAccountRevokedEvent(tenantID, locked.ID, actor, locked.UpdatedAt))
	})
	if err != nil {
		// A writer that does not take the principal lock (cmd/rotator's
		// sweep revokes rows directly) can bump a credential's
		// record_version between ListByPrincipal and Update. If the
		// principal reached `revoked` anyway (a concurrent TS-7), that is
		// the desired end state; otherwise the caller retries the 409.
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.ErrOptimisticLockConflict {
			if latest, findErr := s.principals.FindByID(ctx, tenantID, p.ID); findErr == nil && latest.IsRevoked() {
				return s.revokedPrincipalResult(ctx, latest, 0)
			}
		}
		return nil, s.enrichRotationInFlight(ctx, err, tenantID, p.ID)
	}
	if s.log != nil {
		s.log.Info("principal: revoked", map[string]any{
			"tenant_id": tenantID.String(), "principal_id": revoked.ID.String(),
			"newly_revoked": newlyRevoked,
		})
	}
	return buildRevokedResult(revoked, txCreds, newlyRevoked), nil
}

// revokedPrincipalResult builds TS-7's response from a revoked principal,
// re-reading its credential rows from the database. Used only for the
// pre-lock idempotent path (principal already `revoked` before we took the
// lock), where we have no in-transaction creds list to reuse.
func (s *CredentialService) revokedPrincipalResult(ctx context.Context, p *domain.ServiceAccountPrincipal, newlyRevoked int) (*RevokePrincipalResult, error) {
	creds, err := s.credentials.ListByPrincipal(ctx, p.TenantID, p.ID)
	if err != nil {
		return nil, err
	}
	return buildRevokedResult(p, creds, newlyRevoked), nil
}

// buildRevokedResult assembles the RevokePrincipalResult from a principal and
// its already-fetched credential list (M-19: callers supply the list they read
// inside the transaction instead of letting this function re-read it outside).
func buildRevokedResult(p *domain.ServiceAccountPrincipal, creds []*domain.Credential, newlyRevoked int) *RevokePrincipalResult {
	versions := make([]int, 0, len(creds))
	for _, c := range creds {
		versions = append(versions, c.Version)
	}
	sort.Ints(versions)
	return &RevokePrincipalResult{
		PrincipalID: p.ID, TenantID: p.TenantID, KeycloakClientID: p.KeycloakClientID,
		Status: p.Status, RecordVersion: p.RecordVersion, RevokedAt: p.UpdatedAt,
		Versions: versions, NewlyRevoked: newlyRevoked,
	}
}
