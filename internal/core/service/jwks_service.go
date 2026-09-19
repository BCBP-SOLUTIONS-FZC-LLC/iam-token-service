package service

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

var errNotPEM = errors.New("service: OpenBao material is not PEM-encoded")

// JWKSService serves the public half of a tenant's platform-automation
// keys (EXT-6) — the JWKS Keycloak's client-jwt authenticator fetches
// (lazily, uncached, until an explicit clear-keys-cache) to verify that
// principal's client_assertion. Read-only; never mutates
// principals/credentials/OpenBao.
type JWKSService struct {
	principals  port.PrincipalRepository
	credentials port.CredentialRepository
	secrets     port.SecretStore
	log         port.Logger
}

// NewJWKSService wires JWKSService's dependencies.
func NewJWKSService(principals port.PrincipalRepository, credentials port.CredentialRepository, secrets port.SecretStore, log port.Logger) *JWKSService {
	return &JWKSService{principals: principals, credentials: credentials, secrets: secrets, log: log}
}

// PublicKeys returns the JWK Set entries for every `active`/`rotating`
// credential of tenantID's platform_automation principal — both statuses,
// so a rotated-but-not-yet-swept prior key still verifies during its
// overlap window (§6.2). A `revoked` credential is never included: that is
// the enforcement point that makes RP-17's revoke path (§2.5,
// ClearServiceAccountKeysCache) actually take effect at Keycloak once
// called. Returns an empty (never nil) slice for an unknown tenant/absent
// principal — this is a public, unauthenticated endpoint (Keycloak's own
// outbound fetch carries no caller identity), so existence is never
// signaled via an empty-vs-error distinction.
//
// skipped counts live credentials whose OpenBao material could not be
// read/parsed (production-readiness review, TS-D15) — a credential this
// service believes is live but cannot actually serve. This still degrades
// gracefully (the tenant's OTHER live keys, if any, are still returned —
// dropping the whole response over one bad row would be worse), but a
// silently-partial 200 here means Keycloak cannot validate that specific
// key, i.e. a real per-credential auth outage masquerading as success;
// skipped lets the caller (JWKSHandler) make that observable
// (iam_token_service_jwks_key_errors_total) instead of it only ever
// reaching a log line.
func (s *JWKSService) PublicKeys(ctx context.Context, tenantID uuid.UUID) (keys []map[string]any, skipped int, err error) {
	principals, err := s.principals.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, 0, err
	}

	var principal *domain.ServiceAccountPrincipal
	for _, p := range principals {
		if p.PrincipalType == domain.PrincipalTypePlatformAutomation && p.Status == domain.PrincipalStatusActive {
			principal = p
			break
		}
	}
	if principal == nil {
		return []map[string]any{}, 0, nil
	}

	creds, err := s.credentials.ListByPrincipal(ctx, tenantID, principal.ID)
	if err != nil {
		return nil, 0, err
	}

	keys = make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		if c.Status != domain.CredentialStatusActive && c.Status != domain.CredentialStatusRotating {
			continue
		}
		jwk, jwkErr := s.jwkFor(ctx, c)
		if jwkErr != nil {
			skipped++
			if s.log != nil {
				s.log.Warn("jwks: skipping unreadable credential", map[string]interface{}{
					"tenant_id": tenantID.String(), "principal_id": principal.ID.String(),
					"credential_id": c.ID.String(), "version": c.Version, "error": jwkErr.Error(),
				})
			}
			continue
		}
		keys = append(keys, jwk)
	}
	return keys, skipped, nil
}

// jwkFor reads c's PEM-encoded private key from OpenBao and derives its
// public-key JWK representation (kid=credential id — already a unique,
// existing UUID PK, no new field needed).
func (s *JWKSService) jwkFor(ctx context.Context, c *domain.Credential) (map[string]any, error) {
	pemStr, err := s.secrets.Read(ctx, c.OpenBaoPath)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errNotPEM
	}
	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return jwkForPublicKey(c.ID.String(), &priv.PublicKey), nil
}

func jwkForPublicKey(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"use": "sig",
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}
