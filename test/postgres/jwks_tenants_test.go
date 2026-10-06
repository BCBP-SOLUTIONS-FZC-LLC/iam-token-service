//go:build integration

package postgres_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
)

// TestJWKSTenantsRepository_ListTenantsWithLiveCredentials — TS-D23: the
// JWKS route's known-tenant set is every tenant with an active or rotating
// credential, once each (DISTINCT), across tenants (BYPASSRLS reconciler
// pool, no GUC); a tenant whose credentials are all revoked, or that has a
// principal but no credential, is not in it.
func TestJWKSTenantsRepository_ListTenantsWithLiveCredentials(t *testing.T) {
	t.Parallel()
	_, reconcilerPool, rawPool := setupTestDB(t)
	ctx := context.Background()

	activeAndRotating := uuid.New()
	p := seedPrincipal(t, ctx, rawPool, activeAndRotating)
	seedCredential(t, ctx, rawPool, activeAndRotating, p, 1, "rotating")
	seedCredential(t, ctx, rawPool, activeAndRotating, p, 2, "active")

	onlyRotating := uuid.New()
	p = seedPrincipal(t, ctx, rawPool, onlyRotating)
	seedCredential(t, ctx, rawPool, onlyRotating, p, 1, "revoked")
	seedCredential(t, ctx, rawPool, onlyRotating, p, 2, "rotating")

	onlyRevoked := uuid.New()
	p = seedPrincipal(t, ctx, rawPool, onlyRevoked)
	seedCredential(t, ctx, rawPool, onlyRevoked, p, 1, "revoked")

	seedPrincipal(t, ctx, rawPool, uuid.New()) // principal, no credential

	ids, err := pgadapter.NewJWKSTenantsRepository(reconcilerPool).ListTenantsWithLiveCredentials(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{activeAndRotating, onlyRotating}, ids)
}

// An empty table is an empty set, not an error.
func TestJWKSTenantsRepository_Empty(t *testing.T) {
	t.Parallel()
	_, reconcilerPool, _ := setupTestDB(t)
	ids, err := pgadapter.NewJWKSTenantsRepository(reconcilerPool).ListTenantsWithLiveCredentials(context.Background())
	require.NoError(t, err)
	assert.Empty(t, ids)
}
