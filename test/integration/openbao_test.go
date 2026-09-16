//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
)

// TestOpenBao_Health — Health succeeds against a real, correctly
// bootstrapped container (exercises the same Kubernetes-auth login path
// every other call depends on, §14.4).
func TestOpenBao_Health(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env)

	require.NoError(t, client.Health(context.Background()))
}

// TestOpenBao_WriteReadRoundTrip — Write then Read returns the exact
// secret string, at the real frozen path shape (§6.3/§25).
func TestOpenBao_WriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env)
	ctx := context.Background()

	tenantID := uuid.New()
	path := domain.OpenBaoPathFor(tenantID, "platform-automation", 1)
	const secret = "s3cr3t-material-testonly"

	require.NoError(t, client.Write(ctx, path, secret))

	got, err := client.Read(ctx, path)
	require.NoError(t, err)
	assert.Equal(t, secret, got)
}

// TestOpenBao_ReadNeverWritten — a Read on a path that was never written
// surfaces a real OpenBao 404, correctly mapped to
// domain.ErrSecretStoreUnavailable (§5.4 TS-1) — proves isNotFound/wrapErr
// work against the REAL wire format, not just a hand-rolled unit-test
// fake.
func TestOpenBao_ReadNeverWritten(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env)
	ctx := context.Background()

	path := domain.OpenBaoPathFor(uuid.New(), "platform-automation", 1)
	_, err := client.Read(ctx, path)
	require.Error(t, err)

	var de *domain.Error
	require.True(t, errors.As(err, &de), "expected *domain.Error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
}

// TestOpenBao_DeleteIsIdempotent — Delete then Read is a real not-found;
// Delete on an already-deleted/never-written path is a no-op success, not
// an error (CUST-2 — revoke/offboarding/the §8.6 reconciler all depend on
// this idempotency).
func TestOpenBao_DeleteIsIdempotent(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env)
	ctx := context.Background()

	tenantID := uuid.New()
	path := domain.OpenBaoPathFor(tenantID, "platform-automation", 1)
	require.NoError(t, client.Write(ctx, path, "material"))

	require.NoError(t, client.Delete(ctx, path))

	_, err := client.Read(ctx, path)
	require.Error(t, err)
	var de *domain.Error
	require.True(t, errors.As(err, &de))
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)

	// Deleting again (already gone) — and deleting a path that was never
	// written at all — must both be no-ops, not errors.
	require.NoError(t, client.Delete(ctx, path))
	require.NoError(t, client.Delete(ctx, domain.OpenBaoPathFor(uuid.New(), "platform-automation", 1)))
}

// TestOpenBao_ListEnumeratesVersions — List under a principal's prefix
// returns every version segment written; List on an empty/never-written
// prefix returns an empty slice, not an error (§8.6 orphan-material
// reconciler).
func TestOpenBao_ListEnumeratesVersions(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env)
	ctx := context.Background()

	tenantID := uuid.New()
	const clientID = "platform-automation"
	prefix := "iam/serviceaccount/" + tenantID.String() + "/" + clientID

	for v := 1; v <= 3; v++ {
		require.NoError(t, client.Write(ctx, domain.OpenBaoPathFor(tenantID, clientID, v), "material"))
	}

	keys, err := client.List(ctx, prefix)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"v1", "v2", "v3"}, keys)

	// Empty prefix — nothing written under a different, unused tenant.
	emptyPrefix := "iam/serviceaccount/" + uuid.New().String() + "/" + clientID
	empty, err := client.List(ctx, emptyPrefix)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// TestOpenBao_PermissionDenied_SurfacesRealError — a call against a path
// OUTSIDE the policy's granted "iam/serviceaccount/*" prefix, authenticated
// with the real iam-token-service role, is rejected by OpenBao itself
// (deploy/openbao/policy.hcl scopes exactly "iam/data|metadata/
// serviceaccount/*") — proves the ACL is actually enforced end-to-end for
// this client, not merely trusted. The client's own Write/Read take the
// full path per call (no separate path-prefix Config knob), so the
// out-of-policy path is passed directly.
func TestOpenBao_PermissionDenied_SurfacesRealError(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env)
	ctx := context.Background()

	outOfPolicyPath := "iam/serviceaccount-other/" + uuid.New().String() + "/platform-automation/v1"

	err := client.Write(ctx, outOfPolicyPath, "should-be-denied")
	require.Error(t, err, "a write outside the policy's granted path must be rejected by OpenBao, not silently succeed")
	var de *domain.Error
	require.True(t, errors.As(err, &de), "expected a correctly-wrapped *domain.Error, not a panic or a raw SDK error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)

	_, err = client.Read(ctx, outOfPolicyPath)
	require.Error(t, err, "a read outside the policy's granted path must also be rejected")
}

// TestOpenBao_TokenIsCachedAcrossCalls — the Kubernetes-auth login only
// happens once per the client's cached-token lifetime: several calls in
// quick succession must not each re-authenticate. Verified independently
// of the adapter's own internals by counting requests the fake TokenReview
// server (the only place a real login is observable from outside the
// container) actually received.
func TestOpenBao_TokenIsCachedAcrossCalls(t *testing.T) {
	t.Parallel()
	env := setupOpenBao(t)
	client := newClient(env)
	ctx := context.Background()

	require.Equal(t, int64(0), env.loginCount.Load(), "no login should have happened yet — setupOpenBao's bootstrap calls use the root token, not Kubernetes auth")

	tenantID := uuid.New()
	require.NoError(t, client.Write(ctx, domain.OpenBaoPathFor(tenantID, "platform-automation", 1), "one"))
	afterFirst := env.loginCount.Load()
	assert.Equal(t, int64(1), afterFirst, "the first call must trigger exactly one Kubernetes-auth login")

	require.NoError(t, client.Write(ctx, domain.OpenBaoPathFor(tenantID, "platform-automation", 2), "two"))
	require.NoError(t, client.Health(ctx))
	assert.Equal(t, afterFirst, env.loginCount.Load(),
		"subsequent calls within the cached token's lifetime (role token_ttl 1h, 30s clock-skew margin) must reuse the cached token, not re-login")
}
