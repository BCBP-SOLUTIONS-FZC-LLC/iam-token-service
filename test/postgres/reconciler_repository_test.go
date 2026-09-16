//go:build integration

package postgres_test

import (
	"context"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReconcilerRepository_ListExpiredRotating — §8.3: only a `rotating`
// row whose expires_at has passed is returned; an `active` row and a
// not-yet-expired `rotating` row are excluded.
func TestReconcilerRepository_ListExpiredRotating(t *testing.T) {
	t.Parallel()
	_, reconcilerPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)

	seedCredential(t, ctx, rawPool, tenantID, principalID, 1, "active")
	expiredID := seedCredential(t, ctx, rawPool, tenantID, principalID, 2, "rotating")
	_, err := rawPool.Exec(ctx, `UPDATE service_account_credentials SET expires_at = now() - interval '1 minute' WHERE id = $1`, expiredID)
	require.NoError(t, err)

	notYetExpiredID := seedCredential(t, ctx, rawPool, tenantID, principalID, 3, "rotating")
	_, err = rawPool.Exec(ctx, `UPDATE service_account_credentials SET expires_at = now() + interval '1 hour' WHERE id = $1`, notYetExpiredID)
	require.NoError(t, err)

	repo := pgadapter.NewReconcilerRepository(reconcilerPool)
	rows, err := repo.ListExpiredRotating(ctx)
	require.NoError(t, err)

	require.Len(t, rows, 1)
	assert.Equal(t, tenantID, rows[0].TenantID)
	assert.Equal(t, principalID, rows[0].PrincipalID)
	assert.Equal(t, 2, rows[0].Version)
}

// TestReconcilerRepository_ListPrincipalMaterialStates — §8.6: MaxVersion
// and CommittedVersions reflect exactly the committed rows across tenants;
// a principal with no credential rows yields MaxVersion=0 and an empty set.
func TestReconcilerRepository_ListPrincipalMaterialStates(t *testing.T) {
	t.Parallel()
	_, reconcilerPool, rawPool := setupTestDB(t)
	ctx := context.Background()

	tenantA := uuid.New()
	principalA := seedPrincipal(t, ctx, rawPool, tenantA)
	seedCredential(t, ctx, rawPool, tenantA, principalA, 1, "rotating")
	seedCredential(t, ctx, rawPool, tenantA, principalA, 2, "active")

	tenantB := uuid.New()
	principalB := seedPrincipal(t, ctx, rawPool, tenantB)
	// principalB has no credential rows at all.

	repo := pgadapter.NewReconcilerRepository(reconcilerPool)
	states, err := repo.ListPrincipalMaterialStates(ctx)
	require.NoError(t, err)

	byPrincipal := make(map[uuid.UUID]struct {
		maxVersion int
		versions   []int
		clientID   string
	})
	for _, st := range states {
		byPrincipal[st.PrincipalID] = struct {
			maxVersion int
			versions   []int
			clientID   string
		}{st.MaxVersion, st.CommittedVersions, st.KeycloakClientID}
	}

	a, ok := byPrincipal[principalA]
	require.True(t, ok)
	assert.Equal(t, 2, a.maxVersion)
	assert.ElementsMatch(t, []int{1, 2}, a.versions)
	assert.Equal(t, testKeycloakClientID, a.clientID)

	b, ok := byPrincipal[principalB]
	require.True(t, ok)
	assert.Equal(t, 0, b.maxVersion)
	assert.Empty(t, b.versions)
}

// TestReconcilerRepository_ListExpiredRotating_EmptyWhenNothingExpired —
// the "nothing to sweep" case: an `active` row and a not-yet-expired
// `rotating` row both exist, but the enumeration returns an empty slice,
// not an error.
func TestReconcilerRepository_ListExpiredRotating_EmptyWhenNothingExpired(t *testing.T) {
	t.Parallel()
	_, reconcilerPool, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	seedCredential(t, ctx, rawPool, tenantID, principalID, 1, "active")

	repo := pgadapter.NewReconcilerRepository(reconcilerPool)
	rows, err := repo.ListExpiredRotating(ctx)
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestReconcilerRepository_ListPrincipalMaterialStates_ThreeVersionsCrossTenant
// — a principal with 3+ committed versions reports the correct MaxVersion
// and CommittedVersions set, and rows from multiple tenants are enumerated
// together in one call (the whole point of the BYPASSRLS reconciler pool,
// §8.6) rather than requiring one call per tenant.
func TestReconcilerRepository_ListPrincipalMaterialStates_ThreeVersionsCrossTenant(t *testing.T) {
	t.Parallel()
	_, reconcilerPool, rawPool := setupTestDB(t)
	ctx := context.Background()

	tenantA := uuid.New()
	principalA := seedPrincipal(t, ctx, rawPool, tenantA)
	seedCredential(t, ctx, rawPool, tenantA, principalA, 1, "revoked")
	seedCredential(t, ctx, rawPool, tenantA, principalA, 2, "rotating")
	seedCredential(t, ctx, rawPool, tenantA, principalA, 3, "active")

	tenantC := uuid.New()
	principalC := seedPrincipal(t, ctx, rawPool, tenantC)
	seedCredential(t, ctx, rawPool, tenantC, principalC, 1, "active")

	repo := pgadapter.NewReconcilerRepository(reconcilerPool)
	states, err := repo.ListPrincipalMaterialStates(ctx)
	require.NoError(t, err)

	seenA, seenC := false, false
	for _, st := range states {
		switch st.PrincipalID {
		case principalA:
			seenA = true
			assert.Equal(t, 3, st.MaxVersion)
			assert.ElementsMatch(t, []int{1, 2, 3}, st.CommittedVersions)
		case principalC:
			seenC = true
			assert.Equal(t, 1, st.MaxVersion)
		}
	}
	assert.True(t, seenA, "tenant A's principal must appear in the cross-tenant enumeration")
	assert.True(t, seenC, "tenant C's principal must appear in the SAME cross-tenant enumeration call")
}
