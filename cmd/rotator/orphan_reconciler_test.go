package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

func materialState(tenantID, principalID uuid.UUID, maxVersion int, committed ...int) port.PrincipalMaterialState {
	return port.PrincipalMaterialState{
		TenantID: tenantID, PrincipalID: principalID, KeycloakClientID: domain.KeycloakClientPlatformAutomation,
		MaxVersion: maxVersion, CommittedVersions: committed,
	}
}

func prefixFor(tenantID uuid.UUID) string {
	return "iam/serviceaccount/" + tenantID.String() + "/" + domain.KeycloakClientPlatformAutomation
}

func TestOrphanReconciler_DeletesBelowMaxUncommittedVersion(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "v2", "v3"}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(tenantID, principalID, 3, 3), // only v3 is committed; v1,v2 are orphans below max
	}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, nil)

	assert.Equal(t, 2, result.OrphanDeleted)
	assert.Equal(t, 1, result.OK)
	assert.Equal(t, 0, result.MissingMaterial)
	assert.ElementsMatch(t, []string{
		domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1),
		domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 2),
	}, secrets.deleted)
}

func TestOrphanReconciler_LeavesUncommittedAtOrAboveMaxVersion(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	// v2 is uncommitted but >= MaxVersion (2) — may belong to a
	// concurrently-committing rotate; must NOT be deleted (§8.6).
	secrets.byPrefix[prefixFor(tenantID)] = []string{"v1", "v2"}

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(tenantID, principalID, 2, 1),
	}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, nil)

	assert.Equal(t, 0, result.OrphanDeleted)
	assert.Equal(t, 1, result.OK)
	assert.Empty(t, secrets.deleted)
}

func TestOrphanReconciler_DetectsMissingMaterial(t *testing.T) {
	secrets := newFakeSecretStore()
	tenantID, principalID := uuid.New(), uuid.New()
	// v1 is committed but OpenBao has nothing at all under this prefix.
	secrets.byPrefix[prefixFor(tenantID)] = nil

	rr := &fakeReconcilerRepository{states: []port.PrincipalMaterialState{
		materialState(tenantID, principalID, 1, 1),
	}}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, nil)

	assert.Equal(t, 1, result.MissingMaterial)
	assert.Equal(t, 0, result.OrphanDeleted)
	assert.Equal(t, 0, result.OK)
}

func TestOrphanReconciler_NoPrincipalsIsNoOp(t *testing.T) {
	secrets := newFakeSecretStore()
	rr := &fakeReconcilerRepository{}

	result := runOrphanMaterialReconciler(context.Background(), rr, secrets, nil)

	assert.Equal(t, materialReconcileResult{}, result)
}

func TestParseVersionKey(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"v1", 1, true},
		{"v42/", 42, true},
		{"metadata", 0, false},
		{"v0", 0, false},
		{"vX", 0, false},
	}
	for _, c := range cases {
		got, ok := parseVersionKey(c.in)
		assert.Equal(t, c.wantOK, ok, "input %q", c.in)
		if c.wantOK {
			assert.Equal(t, c.want, got, "input %q", c.in)
		}
	}
}
