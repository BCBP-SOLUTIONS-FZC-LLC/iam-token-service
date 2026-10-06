package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"

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

	keys, skipped, err := publicKeys(context.Background(), svc, uuid.New())
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

	keys, skipped, err := publicKeys(context.Background(), svc, tenantID)
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

	keys, skipped, err := publicKeys(context.Background(), svc, tenantID)
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

	keys, skipped, err := publicKeys(context.Background(), svc, tenantID)
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

	keys, skipped, err := publicKeys(context.Background(), svc, tenantID)
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

	keys, skipped, err := publicKeys(context.Background(), svc, tenantID)
	require.NoError(t, err)
	assert.Empty(t, keys)
	assert.Equal(t, 1, skipped)
}

func TestJWKSService_PublicKeys_PrincipalRepositoryError_Propagates(t *testing.T) {
	svc, principals, _, _ := newTestJWKSService(t)
	principals.forceListByTenantErr = errors.New("db unavailable")

	_, _, err := publicKeys(context.Background(), svc, uuid.New())
	require.Error(t, err)
}

func TestJWKSService_PublicKeys_CredentialRepositoryError_Propagates(t *testing.T) {
	svc, principals, credentials, _ := newTestJWKSService(t)
	tenantID := uuid.New()
	seedPrincipal(t, principals, tenantID)
	credentials.listByPrincipalErr = errors.New("db unavailable")

	_, _, err := publicKeys(context.Background(), svc, tenantID)
	require.Error(t, err)
}

// A rotating key whose overlap already closed is no longer served, even
// before the sweep revokes it — a Keycloak unknown-kid re-fetch must not
// re-learn a key that is meant to be gone.
func TestJWKSService_PublicKeys_SkipsExpiredOverlap(t *testing.T) {
	svc, principals, credentials, secrets := newTestJWKSService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	expired := seedCredential(t, credentials, secrets, tenantID, p.ID, 1, domain.CredentialStatusRotating)
	past := time.Now().Add(-time.Minute)
	expired.ExpiresAt = &past
	require.NoError(t, credentials.Update(context.Background(), expired))
	active := seedCredential(t, credentials, secrets, tenantID, p.ID, 2, domain.CredentialStatusActive)

	keys, skipped, err := publicKeys(context.Background(), svc, tenantID)
	require.NoError(t, err)
	assert.Zero(t, skipped)
	require.Len(t, keys, 1)
	assert.Equal(t, active.ID.String(), keys[0]["kid"])
}

// cancelingSecretStore cancels the request on its first Read and fails it —
// an OpenBao read cut short because the caller went away.
type cancelingSecretStore struct {
	*fakeSecretStore
	cancel context.CancelFunc
}

func (c *cancelingSecretStore) Read(context.Context, string) (string, error) {
	c.cancel()
	return "", domain.NewError(domain.ErrSecretStoreUnavailable, "openbao: read_secret: context canceled")
}

// A read that failed because the request ended is not a key error: no skip
// counted, no "unreadable credential" warning — the ctx error is returned.
func TestJWKSService_PublicKeys_CanceledRequestIsNotAKeyError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	base := newFakeSecretStore()
	log := &fakeLogger{}
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	seedCredential(t, credentials, base, tenantID, p.ID, 1, domain.CredentialStatusActive)

	ctx, cancel := context.WithCancel(context.Background())
	svc := service.NewJWKSService(principals, credentials, &cancelingSecretStore{fakeSecretStore: base, cancel: cancel}, log)
	keys, skipped, err := publicKeys(ctx, svc, tenantID)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, keys)
	assert.Zero(t, skipped)
	assert.Empty(t, log.warnings)
}

// The skip warning carries trace_id like every other service log line.
func TestJWKSService_PublicKeys_SkipWarningCarriesTraceID(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	log := &fakeLogger{}
	svc := service.NewJWKSService(principals, credentials, newFakeSecretStore(), log)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	require.NoError(t, credentials.Insert(context.Background(), &domain.Credential{
		ID: uuid.New(), TenantID: tenantID, PrincipalID: p.ID, Version: 1, Status: domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, "platform-automation", 1), GrantedBy: domain.SystemPrincipalID,
	}))

	traceID, _ := oteltrace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := oteltrace.SpanIDFromHex("0123456789abcdef")
	ctx := oteltrace.ContextWithSpanContext(context.Background(), oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: oteltrace.FlagsSampled,
	}))
	_, skipped, err := publicKeys(ctx, svc, tenantID)
	require.NoError(t, err)
	require.Equal(t, 1, skipped)
	require.Len(t, log.warnings, 1)
	assert.Equal(t, traceID.String(), log.warnings[0].fields["trace_id"])
}

// publicKeys unwraps PublicKeys' JWKSResult into (keys, skipped, err) for
// the assertions above, which predate the result struct.
func publicKeys(ctx context.Context, svc *service.JWKSService, tenantID uuid.UUID) ([]map[string]any, int, error) {
	res, err := svc.PublicKeys(ctx, tenantID)
	if err != nil {
		return nil, 0, err
	}
	return res.Keys, res.Skipped, nil
}

// TS-D23: losing only an overlap (rotating) key is a degraded but servable
// set; losing the ACTIVE key is not — the key Keycloak must verify today's
// client_assertion with would be missing from what it caches.
func TestJWKSService_PublicKeys_FlagsActiveSkipped(t *testing.T) {
	t.Run("rotating unreadable: servable partial set", func(t *testing.T) {
		svc, principals, credentials, secrets := newTestJWKSService(t)
		tenantID := uuid.New()
		p := seedPrincipal(t, principals, tenantID)
		seedCredential(t, credentials, secrets, tenantID, p.ID, 2, domain.CredentialStatusActive)
		require.NoError(t, credentials.Insert(context.Background(), &domain.Credential{
			ID: uuid.New(), TenantID: tenantID, PrincipalID: p.ID, Version: 1, Status: domain.CredentialStatusRotating,
			OpenBaoPath: domain.OpenBaoPathFor(tenantID, "platform-automation", 1), GrantedBy: domain.SystemPrincipalID,
		}))

		res, err := svc.PublicKeys(context.Background(), tenantID)
		require.NoError(t, err)
		assert.Len(t, res.Keys, 1)
		assert.Equal(t, 1, res.Skipped)
		assert.False(t, res.ActiveSkipped)
		assert.False(t, res.Unservable())
	})
	t.Run("active unreadable: unservable even with an overlap key", func(t *testing.T) {
		svc, principals, credentials, secrets := newTestJWKSService(t)
		tenantID := uuid.New()
		p := seedPrincipal(t, principals, tenantID)
		seedCredential(t, credentials, secrets, tenantID, p.ID, 1, domain.CredentialStatusRotating)
		require.NoError(t, credentials.Insert(context.Background(), &domain.Credential{
			ID: uuid.New(), TenantID: tenantID, PrincipalID: p.ID, Version: 2, Status: domain.CredentialStatusActive,
			OpenBaoPath: domain.OpenBaoPathFor(tenantID, "platform-automation", 2), GrantedBy: domain.SystemPrincipalID,
		}))

		res, err := svc.PublicKeys(context.Background(), tenantID)
		require.NoError(t, err)
		assert.Len(t, res.Keys, 1, "the readable overlap key is still reported")
		assert.Equal(t, 1, res.Skipped)
		assert.True(t, res.ActiveSkipped)
		assert.True(t, res.Unservable())
	})
	t.Run("no live credential: empty but servable", func(t *testing.T) {
		svc, _, _, _ := newTestJWKSService(t)
		res, err := svc.PublicKeys(context.Background(), uuid.New())
		require.NoError(t, err)
		assert.NotNil(t, res.Keys)
		assert.False(t, res.Unservable())
	})
}
