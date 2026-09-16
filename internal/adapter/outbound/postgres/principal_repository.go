package postgres

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const principalColumns = `id, tenant_id, principal_sub, keycloak_client_id, principal_type, status,
	record_version, created_at, updated_at, deleted_at`

// PrincipalRepository implements port.PrincipalRepository against
// `service_account_principals` (§4.2).
type PrincipalRepository struct {
	pool *pgcommon.Pool
}

// NewPrincipalRepository constructs a PrincipalRepository backed by pool.
func NewPrincipalRepository(pool *pgcommon.Pool) *PrincipalRepository {
	return &PrincipalRepository{pool: pool}
}

var _ port.PrincipalRepository = (*PrincipalRepository)(nil)

func scanPrincipal(row pgx.Row) (*domain.ServiceAccountPrincipal, error) {
	var p domain.ServiceAccountPrincipal
	err := row.Scan(&p.ID, &p.TenantID, &p.PrincipalSub, &p.KeycloakClientID, &p.PrincipalType, &p.Status,
		&p.RecordVersion, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// FindByID returns the (tenantID, principalID) principal under the
// caller's RLS-scoped tenant, or domain.ErrPrincipalNotFound (§5.5).
func (r *PrincipalRepository) FindByID(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
	var out *domain.ServiceAccountPrincipal
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT `+principalColumns+` FROM service_account_principals
			WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, principalID, tenantID)
		found, scanErr := scanPrincipal(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.NewError(domain.ErrPrincipalNotFound, "no principal for this tenant")
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

// Register inserts p if no principal yet exists for
// (tenant_id, principal_type) (uq_sap_active_principal); a repeat call
// returns the existing row and created=false (TS-4, §5.4).
func (r *PrincipalRepository) Register(ctx context.Context, p *domain.ServiceAccountPrincipal) (*domain.ServiceAccountPrincipal, bool, error) {
	var out *domain.ServiceAccountPrincipal
	created := false
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO service_account_principals (id, tenant_id, principal_sub, keycloak_client_id, principal_type)
			VALUES (COALESCE($1::uuid, gen_random_uuid()), $2, $3, $4, $5)
			ON CONFLICT (tenant_id, principal_type) DO NOTHING
			RETURNING `+principalColumns,
			nullableUUID(p.ID), p.TenantID, p.PrincipalSub, p.KeycloakClientID, p.PrincipalType)
		found, scanErr := scanPrincipal(row)
		if scanErr == nil {
			created = true
			out = found
			return nil
		}
		if !errors.Is(scanErr, pgx.ErrNoRows) {
			return scanErr
		}
		// Conflict — fetch the existing row for the idempotent-repeat
		// response (TS-4 200, §5.4).
		selRow := tx.QueryRow(ctx, `
			SELECT `+principalColumns+` FROM service_account_principals
			WHERE tenant_id = $1 AND principal_type = $2 AND deleted_at IS NULL`, p.TenantID, p.PrincipalType)
		existing, scanErr := scanPrincipal(selRow)
		if scanErr != nil {
			return scanErr
		}
		out = existing
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// ListByTenant returns every principal row for tenantID (§8.4).
func (r *PrincipalRepository) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*domain.ServiceAccountPrincipal, error) {
	var out []*domain.ServiceAccountPrincipal
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+principalColumns+` FROM service_account_principals
			WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPrincipal(rows)
			if err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteByTenant hard-deletes every principal row for tenantID; the
// composite (principal_id, tenant_id) FK's ON DELETE CASCADE removes its
// credential rows with it (§4.2, §8.4 offboarding cascade).
func (r *PrincipalRepository) DeleteByTenant(ctx context.Context, tenantID uuid.UUID) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM service_account_principals WHERE tenant_id = $1`, tenantID)
		return err
	})
}

// nullableUUID returns nil for the zero uuid.UUID so the INSERT's
// gen_random_uuid() default fires, or id.String() otherwise — pgx accepts
// either through a $1::uuid parameter via COALESCE.
func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
