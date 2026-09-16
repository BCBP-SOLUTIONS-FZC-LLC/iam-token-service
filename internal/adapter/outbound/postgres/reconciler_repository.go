package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/jackc/pgx/v5"
)

// ReconcilerRepository implements port.ReconcilerRepository. The pool
// passed to NewReconcilerRepository MUST be bound to the
// `serviceaccount_reconciler` role (NOLOGIN BYPASSRLS, SELECT-only, no
// GUCProvider — §4.3/RLS-7); this type performs no RLS-scoping of its own
// and relies entirely on the connection's privileges being enumeration-only.
type ReconcilerRepository struct {
	pool *pgcommon.Pool
}

// NewReconcilerRepository constructs a ReconcilerRepository backed by pool
// (the BYPASSRLS reconciler pool — see cmd/rotator's composition root).
func NewReconcilerRepository(pool *pgcommon.Pool) *ReconcilerRepository {
	return &ReconcilerRepository{pool: pool}
}

var _ port.ReconcilerRepository = (*ReconcilerRepository)(nil)

// ListExpiredRotating enumerates every `rotating` credential across all
// tenants whose expires_at has passed (§8.3) — the exact partial index
// idx_sac_overlap exists for.
func (r *ReconcilerRepository) ListExpiredRotating(ctx context.Context) ([]port.ExpiredRotatingCredential, error) {
	var out []port.ExpiredRotatingCredential
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tenant_id, principal_id, version, openbao_path
			FROM service_account_credentials
			WHERE status = 'rotating' AND expires_at IS NOT NULL AND expires_at < now() AND deleted_at IS NULL`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row port.ExpiredRotatingCredential
			if err := rows.Scan(&row.TenantID, &row.PrincipalID, &row.Version, &row.OpenBaoPath); err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListPrincipalMaterialStates enumerates the principal registry across all
// tenants with each principal's committed credential versions (§8.6). A
// principal with no credential rows yet (freshly registered, TS-1 never
// called) yields MaxVersion=0 and an empty CommittedVersions — the
// reconciler then treats every OpenBao entry found under its prefix, if
// any, as an orphan.
func (r *ReconcilerRepository) ListPrincipalMaterialStates(ctx context.Context) ([]port.PrincipalMaterialState, error) {
	var out []port.PrincipalMaterialState
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.tenant_id, p.id, p.keycloak_client_id,
			       COALESCE(MAX(c.version), 0) AS max_version,
			       COALESCE(array_agg(c.version) FILTER (WHERE c.version IS NOT NULL), '{}') AS committed_versions
			FROM service_account_principals p
			LEFT JOIN service_account_credentials c
			       ON c.principal_id = p.id AND c.deleted_at IS NULL
			WHERE p.deleted_at IS NULL
			GROUP BY p.tenant_id, p.id, p.keycloak_client_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st port.PrincipalMaterialState
			if err := rows.Scan(&st.TenantID, &st.PrincipalID, &st.KeycloakClientID, &st.MaxVersion, &st.CommittedVersions); err != nil {
				return err
			}
			out = append(out, st)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
