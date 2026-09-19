package service

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
)

// SecretGenerator produces credential material for a new credential
// version (§6.1, §10.5) — a single CSPRNG-backed keypair generation,
// negligible relative to the OpenBao write it precedes (§21). Despite the
// name (kept for wire/interface stability), this generates an RSA
// keypair, PEM-encoded, not a shared-secret string — see EXT-6: the
// platform-automation principal authenticates to Keycloak via client-jwt
// against a JWKS this service serves (jwks_service.go), not client-secret
// auth, because standard Keycloak client secrets have no rotation-overlap
// mechanism (verified empirically — one secret per client, no grace
// window). The private key is the only material ever written to OpenBao
// or returned by TS-1; the public half is derived from it on demand by
// the JWKS handler, never stored separately (TS-INV-2 still holds: the
// private key crosses the wire in the TS-1 response exactly once).
type SecretGenerator func() (string, error)

// keyBits is the RSA modulus size — 2048 bits, the standard minimum for
// RS256 JWT signing.
const keyBits = 2048

// DefaultKeyGenerator generates an RSA-keyBits keypair and PEM-encodes the
// private key (PKCS1, "RSA PRIVATE KEY") — the form
// x509.ParsePKCS1PrivateKey expects on the read side (jwks_service.go).
func DefaultKeyGenerator() (string, error) {
	priv, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return "", fmt.Errorf("service: generate credential keypair: %w", err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}
	return string(pem.EncodeToMemory(block)), nil
}
