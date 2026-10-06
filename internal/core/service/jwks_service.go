package service

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"time"

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

// JWKSResult is PublicKeys' outcome.
type JWKSResult struct {
	// Keys is the JWK Set entries served (never nil).
	Keys []map[string]any
	// Skipped counts live credentials whose OpenBao material could not be
	// read/parsed (production-readiness review, TS-D15) — see PublicKeys.
	Skipped int
	// ActiveSkipped is true when the skipped credentials include the
	// principal's `active` one (TS-D23) — the key the automation principal
	// is signing with today. Serving the remaining (overlap) keys without
	// it would make Keycloak cache a set that rejects every current
	// client_assertion, so the handler answers 503 instead.
	ActiveSkipped bool
}

// Unservable reports whether the set must not be served as a 200: the
// active key is unreadable, or every live key is (TS-D23). Either way the
// right answer is a 5xx, so Keycloak keeps the keys it already has.
func (r *JWKSResult) Unservable() bool {
	return r.ActiveSkipped || (r.Skipped > 0 && len(r.Keys) == 0)
}

// PublicKeys returns the JWK Set entries for every `active`/`rotating`
// credential of tenantID's platform_automation principal — both statuses,
// so a rotated-but-not-yet-swept prior key still verifies during its
// overlap window (§6.2). A `revoked` credential is never included: that is
// the enforcement point that makes RP-17's revoke path (§2.5,
// ClearServiceAccountKeysCache) actually take effect at Keycloak once
// called. Returns an empty (never nil) key slice for an unknown
// tenant/absent principal — this is a public, unauthenticated endpoint
// (Keycloak's own outbound fetch carries no caller identity), so existence
// is never signaled via an empty-vs-error distinction.
//
// Skipped counts live credentials whose OpenBao material could not be
// read/parsed (production-readiness review, TS-D15) — a credential this
// service believes is live but cannot actually serve. Losing only an
// overlap (`rotating`) key still degrades gracefully: the remaining keys
// are returned (dropping the whole response over one old key would be
// worse), and Skipped lets the caller (JWKSHandler) make that observable
// (iam_token_service_jwks_key_errors_total). Losing the `active` key is
// flagged in ActiveSkipped (TS-D23): see JWKSResult.Unservable. A read
// that failed because ctx ended is not a key error: PublicKeys returns
// ctx.Err() instead.
func (s *JWKSService) PublicKeys(ctx context.Context, tenantID uuid.UUID) (*JWKSResult, error) {
	principals, err := s.principals.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	var principal *domain.ServiceAccountPrincipal
	for _, p := range principals {
		if p.PrincipalType == domain.PrincipalTypePlatformAutomation && p.Status == domain.PrincipalStatusActive {
			principal = p
			break
		}
	}
	if principal == nil {
		return &JWKSResult{Keys: []map[string]any{}}, nil
	}

	creds, err := s.credentials.ListByPrincipal(ctx, tenantID, principal.ID)
	if err != nil {
		return nil, err
	}

	res := &JWKSResult{Keys: make([]map[string]any, 0, len(creds))}
	now := time.Now().UTC()
	for _, c := range creds {
		if c.Status != domain.CredentialStatusActive && c.Status != domain.CredentialStatusRotating {
			continue
		}
		// A rotating key whose overlap has already closed is revoked by the
		// next sweep; until then it must not be served, or a Keycloak
		// unknown-kid re-fetch could re-learn a key that is meant to be gone.
		if c.IsExpiredOverlap(now) {
			continue
		}
		jwk, jwkErr := s.jwkFor(ctx, c)
		if jwkErr != nil {
			// The caller went away (client disconnect, request deadline):
			// the read failed because of that, not because the credential's
			// material is unreadable — it is not a key error, and there is
			// no one left to serve a partial set to.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			res.Skipped++
			if c.Status == domain.CredentialStatusActive {
				res.ActiveSkipped = true
			}
			if s.log != nil {
				s.log.Warn("jwks: skipping unreadable credential", withTraceID(ctx, map[string]interface{}{
					"tenant_id": tenantID.String(), "principal_id": principal.ID.String(),
					"credential_id": c.ID.String(), "version": c.Version, "status": string(c.Status),
					"error": jwkErr.Error(),
				}))
			}
			continue
		}
		res.Keys = append(res.Keys, jwk)
	}
	return res, nil
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
