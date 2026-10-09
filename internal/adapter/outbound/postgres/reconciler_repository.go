package postgres

import (
	"context"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
	"github.com/google/uuid"
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
func (r *ReconcilerRepository) ListExpiredRotating(ctx context.Context, limit int) ([]port.ExpiredRotatingCredential, error) {
	var out []port.ExpiredRotatingCredential
	err := withReadOnlyPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tenant_id, principal_id, version, openbao_path
			FROM service_account_credentials
			WHERE status = 'rotating' AND expires_at IS NOT NULL AND expires_at < now() AND deleted_at IS NULL
			ORDER BY expires_at, id
			LIMIT $1`, limit)
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

// ListDueForRotation enumerates every `active` credential across all
// tenants whose next_rotation_at has passed (§16 TSQ-6 Resolved) — the
// exact partial index idx_sac_next_rotation exists for.
func (r *ReconcilerRepository) ListDueForRotation(ctx context.Context, limit int) ([]port.DueForRotation, error) {
	var out []port.DueForRotation
	err := withReadOnlyPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tenant_id, principal_id, version
			FROM service_account_credentials
			WHERE status = 'active' AND next_rotation_at IS NOT NULL AND next_rotation_at < now() AND deleted_at IS NULL
			ORDER BY next_rotation_at, id
			LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row port.DueForRotation
			if err := rows.Scan(&row.TenantID, &row.PrincipalID, &row.Version); err != nil {
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
// tenants with every credential row (§8.6). A principal with no credential
// rows yet (freshly registered, TS-1 never called, or a TS-1 that crashed
// between its OpenBao write and its commit) yields MaxVersion=0 and no
// Credentials. This is a lock-free snapshot: cmd/rotator treats material no
// row claims here only as an orphan CANDIDATE, and deletes it after taking
// the principal's row lock (the lock TS-1 holds across its OpenBao write
// and commit) and re-checking under it that still no row claims the
// version — not by comparing against MaxVersion.
func (r *ReconcilerRepository) ListPrincipalMaterialStates(ctx context.Context) ([]port.PrincipalMaterialState, error) {
	var out []port.PrincipalMaterialState
	err := withReadOnlyPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.tenant_id, p.id, p.keycloak_client_id,
			       c.version, c.openbao_path, c.status IN ('active', 'rotating')
			FROM service_account_principals p
			LEFT JOIN service_account_credentials c
			       ON c.principal_id = p.id AND c.deleted_at IS NULL
			WHERE p.deleted_at IS NULL
			ORDER BY p.id, c.version`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var cur *port.PrincipalMaterialState
		for rows.Next() {
			var (
				st      port.PrincipalMaterialState
				version *int
				path    *string
				live    *bool
			)
			if err := rows.Scan(&st.TenantID, &st.PrincipalID, &st.KeycloakClientID, &version, &path, &live); err != nil {
				return err
			}
			if cur == nil || cur.PrincipalID != st.PrincipalID {
				out = append(out, st)
				cur = &out[len(out)-1]
			}
			if version == nil {
				continue // principal with no credential rows (LEFT JOIN)
			}
			cur.Credentials = append(cur.Credentials, port.CredentialMaterial{
				Version: *version, OpenBaoPath: deref(path), Live: live != nil && *live,
			})
			cur.MaxVersion = max(cur.MaxVersion, *version)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RotationOverlapCount returns the number of `rotating` credentials that have
// an overlap window set (expires_at IS NOT NULL) and have not been soft-deleted
// across all tenants. Used by cmd/server's rotation-overlap gauge exporter to
// populate iam_token_service_rotation_overlap_active (§11.2). Runs over the
// BYPASSRLS reconciler pool — the pool MUST be bound to the
// serviceaccount_reconciler role (no GUCProvider, no RLS scoping).
func (r *ReconcilerRepository) RotationOverlapCount(ctx context.Context) (int, error) {
	var count int
	// Uses pgcommon.RunInTx directly with a read-only access mode rather than
	// withPool (which defaults to read-write). The reconciler pool has no
	// GUCProvider, so the transaction injects nothing; read-only signals intent
	// and avoids acquiring a row-lock budget on PgBouncer.
	err := pgcommon.RunInTx(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(_ context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*)
			FROM service_account_credentials
			WHERE status = 'rotating'
			  AND expires_at IS NOT NULL
			  AND deleted_at IS NULL`).Scan(&count)
	})
	return count, wrapConnErrCtx(ctx, err)
}

// IsCredentialLive reports whether the credential is still `active` or
// `rotating` right now (false when it was revoked or no longer exists).
func (r *ReconcilerRepository) IsCredentialLive(ctx context.Context, principalID uuid.UUID, version int) (bool, error) {
	var live bool
	err := withReadOnlyPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM service_account_credentials
			                WHERE principal_id = $1 AND version = $2
			                  AND status IN ('active', 'rotating') AND deleted_at IS NULL)`,
			principalID, version).Scan(&live)
	})
	return live, err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
