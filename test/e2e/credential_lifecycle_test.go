//go:build e2e

package e2e_test

import (
	"net/http"
	"testing"
)

// TestIssue_FirstCredential covers TS-1's first-ever issue for a principal:
// 201, version 1, a non-empty secret, and a ServiceAccountCredentialIssued
// row landed in the outbox.
func TestIssue_FirstCredential(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	principalID := registerPrincipal(t, tenantID)

	resp, body := issueOrRotate(t, tenantID, principalID)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if body.Version != 1 {
		t.Fatalf("version = %d, want 1", body.Version)
	}
	if body.Secret == "" {
		t.Fatal("secret must not be empty")
	}
	if body.OpenBaoPath == "" {
		t.Fatal("openbao_path must not be empty")
	}
	if body.ExpiresPriorAt != nil {
		t.Fatalf("expires_prior_at = %v, want nil on a first issue (no prior version to demote)", body.ExpiresPriorAt)
	}

	ctx := t.Context()
	if n := outboxEventCount(t, ctx, "ServiceAccountCredentialIssued", tenantID); n != 1 {
		t.Fatalf("ServiceAccountCredentialIssued outbox rows = %d, want 1", n)
	}
}

// TestRotate_SecondCredential covers TS-1's rotation path: a second
// issue/rotate call (fresh rotation_id) for the same principal returns
// version 2 and demotes the prior active version, with a
// ServiceAccountCredentialRotated outbox row.
func TestRotate_SecondCredential(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	principalID := registerPrincipal(t, tenantID)

	if resp, _ := issueOrRotate(t, tenantID, principalID); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first issue status = %d, want 201", resp.StatusCode)
	}

	resp, body := issueOrRotate(t, tenantID, principalID)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("rotate status = %d, want 201", resp.StatusCode)
	}
	if body.Version != 2 {
		t.Fatalf("version = %d, want 2", body.Version)
	}
	if body.ExpiresPriorAt == nil {
		t.Fatal("expires_prior_at must be set on a rotation (the prior version was demoted)")
	}

	ctx := t.Context()
	if n := outboxEventCount(t, ctx, "ServiceAccountCredentialRotated", tenantID); n != 1 {
		t.Fatalf("ServiceAccountCredentialRotated outbox rows = %d, want 1", n)
	}
}

// TestIssue_RotationIDReplay covers §9.2: repeating the exact same
// rotation_id returns 200 (not 201) with the SAME version/secret, and does
// NOT enqueue a second outbox event.
func TestIssue_RotationIDReplay(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	principalID := registerPrincipal(t, tenantID)

	rotationID := newRotationID()
	first := issueOrRotateRequestBody{RotationID: rotationID}

	var firstBody issueOrRotateResponseBody
	resp := doJSON(t, http.MethodPost,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String()+"/credentials",
		sysHeaders(tenantID), first, &firstBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first call status = %d, want 201", resp.StatusCode)
	}

	ctx := t.Context()
	before := outboxEventCount(t, ctx, "ServiceAccountCredentialIssued", tenantID)

	var replayBody issueOrRotateResponseBody
	resp = doJSON(t, http.MethodPost,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String()+"/credentials",
		sysHeaders(tenantID), first, &replayBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200", resp.StatusCode)
	}
	if replayBody.Version != firstBody.Version {
		t.Fatalf("replay version = %d, want %d", replayBody.Version, firstBody.Version)
	}
	if replayBody.Secret != firstBody.Secret {
		t.Fatal("replay must return the exact same secret, not generate new material")
	}

	after := outboxEventCount(t, ctx, "ServiceAccountCredentialIssued", tenantID)
	if after != before {
		t.Fatalf("outbox row count changed on replay: before=%d after=%d, want unchanged", before, after)
	}
}

// TestRevoke_ThenIdempotentRepeat covers TS-2: revoking an issued version
// returns 200 with status=revoked and keycloak_invalidation=
// caller_responsibility (TS-INV-7 — this service never invalidates the
// secret at Keycloak); re-revoking the same version is an idempotent
// no-op, not an error, with no duplicate outbox row.
func TestRevoke_ThenIdempotentRepeat(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	principalID := registerPrincipal(t, tenantID)
	if resp, _ := issueOrRotate(t, tenantID, principalID); resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue status = %d, want 201", resp.StatusCode)
	}

	revokePath := "/api/v1/internal/tenants/" + tenantID.String() + "/service-accounts/" + principalID.String() + "/credentials/1/revoke"

	var first revokeResponseBody
	resp := doJSON(t, http.MethodPost, revokePath, sysHeaders(tenantID), nil, &first)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke status = %d, want 200", resp.StatusCode)
	}
	if first.Status != "revoked" {
		t.Fatalf("status = %q, want revoked", first.Status)
	}
	if first.KeycloakInvalidation != "caller_responsibility" {
		t.Fatalf("keycloak_invalidation = %q, want caller_responsibility", first.KeycloakInvalidation)
	}

	ctx := t.Context()
	before := outboxEventCount(t, ctx, "ServiceAccountCredentialRevoked", tenantID)

	var second revokeResponseBody
	resp = doJSON(t, http.MethodPost, revokePath, sysHeaders(tenantID), nil, &second)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-revoke status = %d, want 200 (idempotent)", resp.StatusCode)
	}
	if second.RevokedAt != first.RevokedAt {
		t.Fatalf("re-revoke returned a different revoked_at: got %q, want %q (must be the original)", second.RevokedAt, first.RevokedAt)
	}

	after := outboxEventCount(t, ctx, "ServiceAccountCredentialRevoked", tenantID)
	if after != before {
		t.Fatalf("outbox row count changed on re-revoke: before=%d after=%d, want unchanged", before, after)
	}
}

// TestRevoke_NonexistentVersion covers the 404 path for a version that was
// never issued.
func TestRevoke_NonexistentVersion(t *testing.T) {
	t.Parallel()
	tenantID := newTenantID()
	principalID := registerPrincipal(t, tenantID)

	resp, er := decodeErrorBody(t, http.MethodPost,
		"/api/v1/internal/tenants/"+tenantID.String()+"/service-accounts/"+principalID.String()+"/credentials/99/revoke",
		sysHeaders(tenantID), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if er.Error != "principal_not_found" {
		t.Fatalf("error = %q, want principal_not_found", er.Error)
	}
}

// TestCredentials_CrossTenantIsolation proves RLS on the credential
// endpoints too: tenant B cannot issue against, nor revoke, tenant A's
// principal — even with a syntactically valid principal_id/version — the
// RLS-scoped repository simply cannot see the row, so the result is
// principal_not_found in both directions.
func TestCredentials_CrossTenantIsolation(t *testing.T) {
	t.Parallel()
	tenantA := newTenantID()
	tenantB := newTenantID()
	principalInA := registerPrincipal(t, tenantA)
	if resp, _ := issueOrRotate(t, tenantA, principalInA); resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed issue status = %d, want 201", resp.StatusCode)
	}

	// Issue, scoped to tenant B, against tenant A's principal id.
	var issueBody issueOrRotateResponseBody
	resp := doJSON(t, http.MethodPost,
		"/api/v1/internal/tenants/"+tenantB.String()+"/service-accounts/"+principalInA.String()+"/credentials",
		sysHeaders(tenantB), issueOrRotateRequestBody{RotationID: newRotationID()}, &issueBody)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant issue status = %d, want 404 (RLS must hide tenant A's principal from tenant B)", resp.StatusCode)
	}

	// Revoke, scoped to tenant B, against tenant A's principal id + a real version.
	resp, er := decodeErrorBody(t, http.MethodPost,
		"/api/v1/internal/tenants/"+tenantB.String()+"/service-accounts/"+principalInA.String()+"/credentials/1/revoke",
		sysHeaders(tenantB), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant revoke status = %d, want 404", resp.StatusCode)
	}
	if er.Error != "principal_not_found" {
		t.Fatalf("error = %q, want principal_not_found", er.Error)
	}
}
