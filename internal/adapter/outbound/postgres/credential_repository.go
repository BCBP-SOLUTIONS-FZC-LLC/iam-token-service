package postgres

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// #nosec G101 -- a SQL column list (granted_by/rotation_id are column
// names), not a credential value.
const credentialColumns = `id, tenant_id, principal_id, version, status, openbao_path, granted_by,
	rotation_id, record_version, issued_at, updated_at, rotated_at, expires_at, revoked_at, deleted_at,
	rotation_cadence_days, next_rotation_at`

// CredentialRepository implements port.CredentialRepository against
// `service_account_credentials` (§4.2).
type CredentialRepository struct {
	pool *pgcommon.Pool
}

// NewCredentialRepository constructs a CredentialRepository backed by
// pool.
func NewCredentialRepository(pool *pgcommon.Pool) *CredentialRepository {
	return &CredentialRepository{pool: pool}
}

var _ port.CredentialRepository = (*CredentialRepository)(nil)

func scanCredential(row pgx.Row) (*domain.Credential, error) {
	var c domain.Credential
	err := row.Scan(&c.ID, &c.TenantID, &c.PrincipalID, &c.Version, &c.Status, &c.OpenBaoPath, &c.GrantedBy,
		&c.RotationID, &c.RecordVersion, &c.IssuedAt, &c.UpdatedAt, &c.RotatedAt, &c.ExpiresAt, &c.RevokedAt, &c.DeletedAt,
		&c.RotationCadenceDays, &c.NextRotationAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// FindByVersion reads one credential version under the caller's
// RLS-scoped tenant — TS-2's revoke target lookup.
func (r *CredentialRepository) FindByVersion(ctx context.Context, tenantID, principalID uuid.UUID, version int) (*domain.Credential, error) {
	var out *domain.Credential
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+credentialColumns+` FROM service_account_credentials
			WHERE tenant_id = $1 AND principal_id = $2 AND version = $3 AND deleted_at IS NULL`,
			tenantID, principalID, version)
		found, scanErr := scanCredential(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return nil
			}
			return scanErr
		}
		out = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FindActive returns the principal's current `active` credential, if any
// (§6.2 — at most one at a time, uq_sac_one_active).
func (r *CredentialRepository) FindActive(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.Credential, error) {
	var out *domain.Credential
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+credentialColumns+` FROM service_account_credentials
			WHERE tenant_id = $1 AND principal_id = $2 AND status = 'active' AND deleted_at IS NULL`,
			tenantID, principalID)
		found, scanErr := scanCredential(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return nil
			}
			return scanErr
		}
		out = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FindByRotationID returns the credential row already created for
// (principalID, rotationID), if any — TS-1's idempotency check
// (uq_sac_rotation_id, §9.2).
func (r *CredentialRepository) FindByRotationID(ctx context.Context, tenantID, principalID, rotationID uuid.UUID) (*domain.Credential, error) {
	var out *domain.Credential
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+credentialColumns+` FROM service_account_credentials
			WHERE tenant_id = $1 AND principal_id = $2 AND rotation_id = $3 AND deleted_at IS NULL`,
			tenantID, principalID, rotationID)
		found, scanErr := scanCredential(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return nil
			}
			return scanErr
		}
		out = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListByPrincipal returns every version for principalID, newest first
// (idx_sac_principal) — TS-3's metadata read (§8.5).
func (r *CredentialRepository) ListByPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) ([]*domain.Credential, error) {
	var out []*domain.Credential
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+credentialColumns+` FROM service_account_credentials
			WHERE tenant_id = $1 AND principal_id = $2 AND deleted_at IS NULL
			ORDER BY version DESC`, tenantID, principalID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCredential(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Insert creates a new credential row (an issue, or the new `active`
// version of a rotation). A unique-constraint violation (uq_sac_version,
// uq_sac_one_active, uq_sac_rotation_id) means a concurrent issue/rotate
// under a different rotation_id is racing this one — the service layer
// already checked FindByRotationID for the idempotent-replay case before
// calling Insert, so a collision reaching the database here is a genuine
// concurrent writer, not a replay — mapped to domain.ErrRotationInFlight
// (§5.5/§17) rather than bubbling a raw SQLSTATE.
func (r *CredentialRepository) Insert(ctx context.Context, c *domain.Credential) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		// A SAVEPOINT isolates the INSERT: once any statement inside a
		// transaction fails, Postgres marks the whole transaction aborted
		// and rejects every subsequent statement (25P02) until a ROLLBACK —
		// including the best-effort activeRotationID lookup below, which
		// would otherwise silently fail every single time a real 23505
		// fires, permanently defeating the details.active_rotation_id
		// enrichment (§17) despite this method's own doc comment promising
		// it. RELEASE SAVEPOINT on success is a no-op cost; ROLLBACK TO
		// SAVEPOINT on failure restores the transaction to a usable state
		// without discarding whatever the caller already did earlier in
		// the same tx (e.g. TS-1's demote-to-rotating UPDATE that runs
		// before this INSERT, §9.3).
		if _, spErr := tx.Exec(ctx, `SAVEPOINT credential_insert`); spErr != nil {
			return spErr
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO service_account_credentials
				(tenant_id, principal_id, version, status, openbao_path, granted_by, rotation_id,
				 rotation_cadence_days, next_rotation_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING id, record_version, issued_at, updated_at`,
			c.TenantID, c.PrincipalID, c.Version, c.Status, c.OpenBaoPath, c.GrantedBy, c.RotationID,
			c.RotationCadenceDays, c.NextRotationAt,
		).Scan(&c.ID, &c.RecordVersion, &c.IssuedAt, &c.UpdatedAt)
		if err == nil {
			_, relErr := tx.Exec(ctx, `RELEASE SAVEPOINT credential_insert`)
			return relErr
		}
		if pgcommon.IsUniqueViolation(err) {
			if _, rbErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT credential_insert`); rbErr != nil {
				return rbErr
			}
			de := domain.NewError(domain.ErrRotationInFlight, "a concurrent issue/rotate is already in flight for this principal")
			if activeRotationID, findErr := activeRotationID(ctx, tx, c.TenantID, c.PrincipalID); findErr == nil && activeRotationID != nil {
				de = de.WithDetails(map[string]any{"active_rotation_id": *activeRotationID})
			}
			return de
		}
		return err
	})
}

// activeRotationID best-effort looks up the principal's current active
// credential's rotation_id, for domain.ErrRotationInFlight's
// details.active_rotation_id (§17). A lookup failure is not fatal to the
// caller — Insert still returns ErrRotationInFlight without the detail.
func activeRotationID(ctx context.Context, tx pgx.Tx, tenantID, principalID uuid.UUID) (*uuid.UUID, error) {
	var id *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT rotation_id FROM service_account_credentials
		WHERE tenant_id = $1 AND principal_id = $2 AND status = 'active' AND deleted_at IS NULL`,
		tenantID, principalID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return id, nil
}

// Update applies an optimistic-lock-guarded status transition (WHERE
// id=$1 AND record_version=$2) — demote-to-rotating, revoke, and the §8.3
// sweep's expiry-revoke all go through this one method. Returns
// domain.ErrOptimisticLockConflict on a stale record_version.
//
// rotation_cadence_days/next_rotation_at are written verbatim from c on
// every call (§16 TSQ-6 Resolved) — the service layer nils both fields
// before demoting an `active` row to `rotating`/`revoked` (only the
// current active version is ever "due"), so this is a plain pass-through,
// not conditional logic this repository needs to own.
//
// The touch_row trigger (§4.5) bumps record_version and updated_at
// server-side on every real change; this scans both back into c via
// RETURNING so a caller chaining a second Update on the same in-memory
// credential within one transaction (e.g. rotate-then-later-revoke) sees
// the post-trigger value rather than the stale one it passed in.
func (r *CredentialRepository) Update(ctx context.Context, c *domain.Credential) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE service_account_credentials SET
				status = $3, rotated_at = $4, expires_at = $5, revoked_at = $6,
				rotation_cadence_days = $7, next_rotation_at = $8
			WHERE id = $1 AND record_version = $2
			RETURNING record_version, updated_at`,
			c.ID, c.RecordVersion, c.Status, c.RotatedAt, c.ExpiresAt, c.RevokedAt,
			c.RotationCadenceDays, c.NextRotationAt,
		).Scan(&c.RecordVersion, &c.UpdatedAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrOptimisticLockConflict, "service_account_credentials record_version conflict").
					WithDetails(map[string]any{"expected_version": c.RecordVersion})
			}
			return err
		}
		return nil
	})
}
