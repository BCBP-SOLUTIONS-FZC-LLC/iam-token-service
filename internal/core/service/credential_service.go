// Package service implements the business logic behind TS-1..TS-4 and the
// reconciler jobs — domain + port only, no adapter/Gin/pgx/OpenBao-SDK
// import (§3.2, enforced by .go-arch-lint.yml).
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// CredentialService implements TS-1 (issue/rotate) and TS-2 (revoke)
// (§5.4, §6, §9).
type CredentialService struct {
	principals  port.PrincipalRepository
	credentials port.CredentialRepository
	secrets     port.SecretStore
	tx          port.TxRunner
	log         port.Logger
	generate    SecretGenerator
}

// NewCredentialService wires CredentialService's dependencies. generate
// defaults to DefaultSecretGenerator when nil (tests substitute a
// deterministic stub).
func NewCredentialService(
	principals port.PrincipalRepository,
	credentials port.CredentialRepository,
	secrets port.SecretStore,
	tx port.TxRunner,
	log port.Logger,
	generate SecretGenerator,
) *CredentialService {
	if generate == nil {
		generate = DefaultSecretGenerator
	}
	return &CredentialService{
		principals: principals, credentials: credentials, secrets: secrets,
		tx: tx, log: log, generate: generate,
	}
}

// IssueOrRotateRequest is the TS-1 request body (§5.4).
type IssueOrRotateRequest struct {
	RotationID     uuid.UUID
	OverlapSeconds int
}

// IssueOrRotateResult is the TS-1 response body (§5.4). Replayed is true
// when this result was produced by the rotation_id-idempotency path
// (§9.2) rather than a fresh issue/rotate — the HTTP layer uses it to pick
// 200 vs 201.
type IssueOrRotateResult struct {
	Version        int
	Secret         string
	OpenBaoPath    string
	ExpiresPriorAt *time.Time
	RecordVersion  int
	Replayed       bool
}

// issueOrRotate implements TS-1 (§5.4): issue the first credential for a
// principal or rotate to the next version, returning the generated
// plaintext exactly once (TS-INV-2). Write ordering is material-first,
// then row, then commit (§9.3): an OpenBao failure leaves nothing
// committed in Postgres; a crash between the OpenBao write and the
// Postgres commit leaves a reclaimable orphan (§8.6, CUST-3). Wrapped by
// the exported IssueOrRotate (tracing.go) which starts the
// "credential.issue_rotate" span (§11.3).
func (s *CredentialService) issueOrRotate(ctx context.Context, tenantID, principalID uuid.UUID, req IssueOrRotateRequest, actor uuid.UUID) (*IssueOrRotateResult, error) {
	if req.RotationID == uuid.Nil {
		return nil, domain.NewError(domain.ErrInvalidRequest, "rotation_id is required").
			WithDetails(map[string]any{"field": "rotation_id"})
	}

	principal, err := s.principals.FindByID(ctx, tenantID, principalID)
	if err != nil {
		return nil, err
	}
	if principal.IsRevoked() {
		return nil, domain.NewError(domain.ErrPrincipalRevoked, "principal is revoked; issue/rotate rejected")
	}

	// Idempotency per rotation_id (§9.2): a retried call under the same
	// rotation_id returns the already-committed version and secret without
	// generating new material.
	if existing, err := s.credentials.FindByRotationID(ctx, tenantID, principalID, req.RotationID); err != nil {
		return nil, err
	} else if existing != nil {
		return s.replayIssueOrRotate(ctx, tenantID, principalID, existing)
	}

	overlap := domain.ClampOverlapSeconds(req.OverlapSeconds)

	active, err := s.credentials.FindActive(ctx, tenantID, principalID)
	if err != nil {
		return nil, err
	}

	nextVersion := 1
	if active != nil {
		nextVersion = active.Version + 1
	}
	path := domain.OpenBaoPathFor(tenantID, principal.KeycloakClientID, nextVersion)

	secret, err := s.generate()
	if err != nil {
		return nil, err
	}
	if err := s.secrets.Write(ctx, path, secret); err != nil {
		return nil, err
	}

	rotationID := req.RotationID
	newCred := &domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: nextVersion,
		Status: domain.CredentialStatusActive, OpenBaoPath: path,
		GrantedBy: actor, RotationID: &rotationID,
	}

	now := time.Now().UTC()
	var expiresPrior *time.Time
	err = s.tx.RunInTx(ctx, func(ctx context.Context) error {
		// Demote the prior `active` row to `rotating` BEFORE inserting the
		// new `active` row: uq_sac_one_active is a plain (non-deferrable)
		// partial unique index, checked immediately per-statement — an
		// insert-then-demote ordering would have both rows `active`
		// simultaneously and fail the constraint at INSERT time.
		if active != nil {
			expiresAt := now.Add(time.Duration(overlap) * time.Second)
			active.Status = domain.CredentialStatusRotating
			active.ExpiresAt = &expiresAt
			if err := s.credentials.Update(ctx, active); err != nil {
				return err
			}
			expiresPrior = &expiresAt
		}
		if err := s.credentials.Insert(ctx, newCred); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		if active == nil {
			return pub.Enqueue(ctx, &domain.Event{
				Type: domain.EventServiceAccountCredentialIssued, TenantID: tenantID, Actor: actor,
				Data: domain.ServiceAccountCredentialIssuedPayload{
					TenantID: tenantID, PrincipalID: principalID, Version: nextVersion,
					IssuedAt: newCred.IssuedAt.UTC().Format(time.RFC3339),
				},
			})
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventServiceAccountCredentialRotated, TenantID: tenantID, Actor: actor,
			Data: domain.ServiceAccountCredentialRotatedPayload{
				TenantID: tenantID, PrincipalID: principalID, Version: nextVersion,
				PriorVersion: active.Version, ExpiresPriorAt: expiresPrior.UTC().Format(time.RFC3339),
			},
		})
	})
	if err != nil {
		// Nothing further to compensate: the OpenBao write above is a
		// reclaimable orphan (below the principal's committed max version),
		// left for the §8.6 reconciler rather than deleted here — a
		// same-transaction failure here means the caller never got a
		// version number back, so it cannot yet be "the committed max".
		return nil, err
	}

	s.sweepExpiredRotating(ctx, tenantID, principalID, actor)

	return &IssueOrRotateResult{
		Version: nextVersion, Secret: secret, OpenBaoPath: path,
		ExpiresPriorAt: expiresPrior, RecordVersion: newCred.RecordVersion,
	}, nil
}

// replayIssueOrRotate handles a TS-1 retry under an already-committed
// rotation_id (§9.2): no new material, no new row, no new event — just the
// existing version's secret re-read from OpenBao (port.SecretStore.Read,
// scoped to exactly this replay path).
//
// rotation_id is keyed to the row it created for the lifetime of that row,
// not just "the current in-flight rotation" — a caller may legitimately
// replay a rotation_id from a rotation that has since been superseded by a
// later one. While the row is still `active` or `rotating` (inside its
// overlap window), its OpenBao material is still present and this returns
// the same historical result the original call did — correct idempotency-
// key semantics. Once the row reaches `revoked` (past overlap-expiry, or a
// TS-2/offboarding revoke), its material is gone; letting the Read below
// fail on a missing OpenBao entry would surface as a misleading 502
// secret_store_unavailable (looks like an outage), so that case is
// classified explicitly instead.
func (s *CredentialService) replayIssueOrRotate(ctx context.Context, tenantID, principalID uuid.UUID, existing *domain.Credential) (*IssueOrRotateResult, error) {
	if existing.Status == domain.CredentialStatusRevoked {
		return nil, domain.NewError(domain.ErrCredentialReplayRevoked,
			"rotation_id refers to a credential version that has since been revoked; issue a new rotation_id to rotate again").
			WithDetails(map[string]any{"version": existing.Version})
	}
	secret, err := s.secrets.Read(ctx, existing.OpenBaoPath)
	if err != nil {
		return nil, err
	}
	var expiresPrior *time.Time
	if existing.Version > 1 {
		if prior, err := s.credentials.FindByVersion(ctx, tenantID, principalID, existing.Version-1); err == nil && prior != nil {
			expiresPrior = prior.ExpiresAt
		}
	}
	return &IssueOrRotateResult{
		Version: existing.Version, Secret: secret, OpenBaoPath: existing.OpenBaoPath,
		ExpiresPriorAt: expiresPrior, RecordVersion: existing.RecordVersion, Replayed: true,
	}, nil
}

// RevokeResult is the TS-2 response body (§5.4).
type RevokeResult struct {
	Version   int
	Status    domain.CredentialStatus
	RevokedAt time.Time
}

// revoke implements TS-2 (§5.4): revokes this service's custody/metadata
// half of one credential version — deletes its OpenBao material and marks
// the row `revoked`. Idempotent: re-revoking an already-`revoked` version
// is a no-op 200 (§5.4). Does NOT invalidate the secret at Keycloak
// (two-halves, TS-INV-7, §6.1/§8.7) — that is the caller's (Realm
// Provisioner's) responsibility. Wrapped by the exported Revoke
// (tracing.go) which starts the "credential.revoke" span (§11.3).
func (s *CredentialService) revoke(ctx context.Context, tenantID, principalID uuid.UUID, version int, actor uuid.UUID) (*RevokeResult, error) {
	cred, err := s.credentials.FindByVersion(ctx, tenantID, principalID, version)
	if err != nil {
		return nil, err
	}
	if cred == nil {
		return nil, domain.NewError(domain.ErrPrincipalNotFound, "no credential at this version for this principal")
	}
	return s.revokeCredential(ctx, tenantID, principalID, cred, actor)
}

// revokeCredential is the shared revoke path used by both the public
// Revoke (TS-2) and the opportunistic overlap-expiry sweep (§8.3). Ordering
// mirrors TS-1's material-first discipline in the destructive direction:
// OpenBao material is deleted BEFORE the Postgres commit, so a credential
// row is never left `revoked` while its material still lives (CUST-2). A
// crash between the two leaves a divergence in the opposite direction — a
// non-revoked row whose material is already gone — which the §8.6
// reconciler surfaces as `missing_material` (page-worthy) rather than
// silently losing the fact that a delete was attempted.
func (s *CredentialService) revokeCredential(ctx context.Context, tenantID, principalID uuid.UUID, cred *domain.Credential, actor uuid.UUID) (*RevokeResult, error) {
	if cred.Status == domain.CredentialStatusRevoked {
		revokedAt := time.Time{}
		if cred.RevokedAt != nil {
			revokedAt = *cred.RevokedAt
		}
		return &RevokeResult{Version: cred.Version, Status: cred.Status, RevokedAt: revokedAt}, nil
	}

	if err := s.secrets.Delete(ctx, cred.OpenBaoPath); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	cred.Status = domain.CredentialStatusRevoked
	cred.RevokedAt = &now

	err := s.tx.RunInTx(ctx, func(ctx context.Context) error {
		if err := s.credentials.Update(ctx, cred); err != nil {
			return err
		}
		pub, ok := port.EventPublisherFromContext(ctx)
		if !ok {
			return nil
		}
		return pub.Enqueue(ctx, &domain.Event{
			Type: domain.EventServiceAccountCredentialRevoked, TenantID: tenantID, Actor: actor,
			Data: domain.ServiceAccountCredentialRevokedPayload{
				TenantID: tenantID, PrincipalID: principalID, Version: cred.Version,
				RevokedAt: now.Format(time.RFC3339),
			},
		})
	})
	if err != nil {
		// A concurrent actor (another TS-2 call, or cmd/rotator's
		// overlap-expiry sweep) may have revoked this exact row between our
		// read and this write — record_version no longer matches, so Update
		// reports ErrOptimisticLockConflict even though the caller's desired
		// end state (revoked) was already reached by the other actor. TS-2
		// is documented as idempotent (§5.4); that guarantee must hold
		// against a genuine race, not only a stale read taken before one
		// started. Re-check before surfacing what would otherwise be a
		// misleading 409 for what is, from the caller's perspective, success.
		var de *domain.Error
		if errors.As(err, &de) && de.Code == domain.ErrOptimisticLockConflict {
			if latest, findErr := s.credentials.FindByVersion(ctx, tenantID, principalID, cred.Version); findErr == nil && latest != nil && latest.Status == domain.CredentialStatusRevoked {
				revokedAt := time.Time{}
				if latest.RevokedAt != nil {
					revokedAt = *latest.RevokedAt
				}
				return &RevokeResult{Version: latest.Version, Status: latest.Status, RevokedAt: revokedAt}, nil
			}
		}
		return nil, err
	}
	return &RevokeResult{Version: cred.Version, Status: cred.Status, RevokedAt: now}, nil
}

// sweepExpiredRotating opportunistically revokes principalID's own expired
// `rotating` credentials (§8.3 — "the next TS-1 for the principal also
// opportunistically sweeps its own principal's expired rotating row"),
// under the request's own tenant GUC, no BYPASSRLS. Best-effort: a failure
// here never fails the TS-1 call that triggered it — the cron sweep
// (cmd/rotator, §8.3) is the availability-first backstop, so this is purely
// an optimization that shortens convergence, not a correctness dependency.
func (s *CredentialService) sweepExpiredRotating(ctx context.Context, tenantID, principalID, actor uuid.UUID) {
	creds, err := s.credentials.ListByPrincipal(ctx, tenantID, principalID)
	if err != nil {
		s.warn(ctx, "sweep_expired_rotating: list failed", tenantID, principalID, err)
		return
	}
	now := time.Now().UTC()
	for _, c := range creds {
		if !c.IsExpiredOverlap(now) {
			continue
		}
		if _, err := s.revokeCredential(ctx, tenantID, principalID, c, actor); err != nil {
			s.warn(ctx, "sweep_expired_rotating: revoke failed", tenantID, principalID, err)
		}
	}
}

func (s *CredentialService) warn(ctx context.Context, msg string, tenantID, principalID uuid.UUID, err error) {
	if s.log == nil {
		return
	}
	s.log.Warn(msg, withTraceID(ctx, map[string]interface{}{
		"tenant_id": tenantID, "principal_id": principalID, "error": err.Error(),
	}))
}
