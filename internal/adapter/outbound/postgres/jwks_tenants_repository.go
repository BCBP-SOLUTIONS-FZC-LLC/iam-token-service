package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// JWKSTenantsRepository lists the tenants the JWKS route treats as "known"
// (TS-D23): those with at least one live (active or rotating) credential.
// It needs the BYPASSRLS reconciler pool — the list is cross-tenant by
// definition, and the RLS-scoped app pool would see nothing without a
// tenant GUC. Like RLSViolationRepository it implements no core port: its
// only caller is cmd/server's JWKSHandler.RunKnownTenantRefresher.
type JWKSTenantsRepository struct {
	pool *pgcommon.Pool
}

// NewJWKSTenantsRepository constructs a JWKSTenantsRepository over the
// reconciler pool.
func NewJWKSTenantsRepository(reconcilerPool *pgcommon.Pool) *JWKSTenantsRepository {
	return &JWKSTenantsRepository{pool: reconcilerPool}
}

// ListTenantsWithLiveCredentials returns the DISTINCT tenant ids that have
// an `active` or `rotating` credential. One row per tenant, so its size is
// the tenant count, not the credential count.
func (r *JWKSTenantsRepository) ListTenantsWithLiveCredentials(ctx context.Context) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT DISTINCT tenant_id FROM service_account_credentials
			 WHERE status IN ('active', 'rotating') AND deleted_at IS NULL`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}
