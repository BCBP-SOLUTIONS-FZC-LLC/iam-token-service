package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

func newTestJWKSService(t *testing.T) (*service.JWKSService, *fakePrincipalRepository, *fakeCredentialRepository, *fakeSecretStore) {
	t.Helper()
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	svc := service.NewJWKSService(principals, credentials, secrets, &fakeLogger{})
	return svc, principals, credentials, secrets
}

func seedCredential(t *testing.T, credentials *fakeCredentialRepository, secrets *fakeSecretStore, tenantID, principalID uuid.UUID, version int, status domain.CredentialStatus) *domain.Credential {
	t.Helper()
	pem, err := service.DefaultKeyGenerator()
	require.NoError(t, err)

	c := &domain.Credential{
		ID: uuid.New(), TenantID: tenantID, PrincipalID: principalID,
		Version: version, Status: status,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, "platform-automation", version),
		GrantedBy:   domain.SystemPrincipalID,
	}
	require.NoError(t, credentials.Insert(context.Background(), c))
	require.NoError(t, secrets.Write(context.Background(), c.OpenBaoPath, pem))
	return c
}

func TestJWKSService_PublicKeys_NoPrincipal_ReturnsEmptyNotError(t *testing.T) {
	svc, _, _, _ := newTestJWKSService(t)

	keys, skipped, err := svc.PublicKeys(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, keys)
	assert.Zero(t, skipped)
}

func TestJWKSService_PublicKeys_RevokedPrincipal_ReturnsEmpty(t *testing.T) {
	svc, principals, credentials, secrets := newTestJWKSService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	p.Status = domain.PrincipalStatusRevoked
	principals.put(p)
	seedCredential(t, credentials, secrets, tenantID, p.ID, 1, domain.CredentialStatusActive)

	keys, skipped, err := svc.PublicKeys(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Empty(t, keys, "a revoked principal must never serve keys")
	assert.Zero(t, skipped)
}

func TestJWKSService_PublicKeys_IncludesActiveAndRotating_ExcludesRevoked(t *testing.T) {
	svc, principals, credentials, secrets := newTestJWKSService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	active := seedCredential(t, credentials, secrets, tenantID, p.ID, 2, domain.CredentialStatusActive)
	rotating := seedCredential(t, credentials, secrets, tenantID, p.ID, 1, domain.CredentialStatusRotating)
	revoked := seedCredential(t, credentials, secrets, tenantID, p.ID, 0, domain.CredentialStatusRevoked)

	keys, skipped, err := svc.PublicKeys(context.Background(), tenantID)
	require.NoError(t, err)
	require.Len(t, keys, 2, "active + rotating, revoked excluded")
	assert.Zero(t, skipped)

	kids := map[string]bool{}
	for _, k := range keys {
		kids[k["kid"].(string)] = true
		assert.Equal(t, "RSA", k["kty"])
		assert.Equal(t, "sig", k["use"])
		assert.Equal(t, "RS256", k["alg"])
		assert.NotEmpty(t, k["n"])
		assert.NotEmpty(t, k["e"])
	}
	assert.True(t, kids[active.ID.String()])
	assert.True(t, kids[rotating.ID.String()])
	assert.False(t, kids[revoked.ID.String()])
}

func TestJWKSService_PublicKeys_SkipsUnreadableCredential(t *testing.T) {
	svc, principals, credentials, secrets := newTestJWKSService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	good := seedCredential(t, credentials, secrets, tenantID, p.ID, 2, domain.CredentialStatusActive)
	// A second active-status row whose OpenBao material is missing —
	// mirrors an unexpected divergence between Postgres and OpenBao rather
	// than a state the normal issue/rotate flow can produce.
	bad := &domain.Credential{
		ID: uuid.New(), TenantID: tenantID, PrincipalID: p.ID,
		Version: 1, Status: domain.CredentialStatusRotating,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, "platform-automation", 1),
		GrantedBy:   domain.SystemPrincipalID,
	}
	require.NoError(t, credentials.Insert(context.Background(), bad))

	keys, skipped, err := svc.PublicKeys(context.Background(), tenantID)
	require.NoError(t, err)
	require.Len(t, keys, 1, "the unreadable credential is skipped, not fatal to the whole response")
	assert.Equal(t, good.ID.String(), keys[0]["kid"])
	assert.Equal(t, 1, skipped, "the unreadable credential must be counted, not just logged (TS-D15)")
}

// TestJWKSService_PublicKeys_SkipsNonPEMMaterial covers jwkFor's
// pem.Decode-returns-nil branch: OpenBao material present but not
// PEM-encoded at all, a divergence pem.Decode itself rejects before
// x509 parsing is even attempted.
func TestJWKSService_PublicKeys_SkipsNonPEMMaterial(t *testing.T) {
	svc, principals, credentials, secrets := newTestJWKSService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	bad := &domain.Credential{
		ID: uuid.New(), TenantID: tenantID, PrincipalID: p.ID,
		Version: 1, Status: domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, "platform-automation", 1),
		GrantedBy:   domain.SystemPrincipalID,
	}
	require.NoError(t, credentials.Insert(context.Background(), bad))
	require.NoError(t, secrets.Write(context.Background(), bad.OpenBaoPath, "not a pem at all"))

	keys, skipped, err := svc.PublicKeys(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Empty(t, keys)
	assert.Equal(t, 1, skipped)
}

// TestJWKSService_PublicKeys_SkipsMalformedPKCS1Body covers jwkFor's
// x509.ParsePKCS1PrivateKey error branch: a syntactically valid PEM block
// whose DER body is not a parseable RSA private key.
func TestJWKSService_PublicKeys_SkipsMalformedPKCS1Body(t *testing.T) {
	svc, principals, credentials, secrets := newTestJWKSService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	bad := &domain.Credential{
		ID: uuid.New(), TenantID: tenantID, PrincipalID: p.ID,
		Version: 1, Status: domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, "platform-automation", 1),
		GrantedBy:   domain.SystemPrincipalID,
	}
	require.NoError(t, credentials.Insert(context.Background(), bad))
	malformedPEM := "-----BEGIN RSA PRIVATE KEY-----\n" + "AAAA\n" + "-----END RSA PRIVATE KEY-----\n"
	require.NoError(t, secrets.Write(context.Background(), bad.OpenBaoPath, malformedPEM))

	keys, skipped, err := svc.PublicKeys(context.Background(), tenantID)
	require.NoError(t, err)
	assert.Empty(t, keys)
	assert.Equal(t, 1, skipped)
}

func TestJWKSService_PublicKeys_PrincipalRepositoryError_Propagates(t *testing.T) {
	svc, principals, _, _ := newTestJWKSService(t)
	principals.forceListByTenantErr = errors.New("db unavailable")

	_, _, err := svc.PublicKeys(context.Background(), uuid.New())
	require.Error(t, err)
}

func TestJWKSService_PublicKeys_CredentialRepositoryError_Propagates(t *testing.T) {
	svc, principals, credentials, _ := newTestJWKSService(t)
	tenantID := uuid.New()
	seedPrincipal(t, principals, tenantID)
	credentials.listByPrincipalErr = errors.New("db unavailable")

	_, _, err := svc.PublicKeys(context.Background(), tenantID)
	require.Error(t, err)
}
