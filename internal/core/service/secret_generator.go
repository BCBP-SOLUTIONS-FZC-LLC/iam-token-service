package service

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// SecretGenerator produces cryptographically-random secret material for a
// new credential version (§6.1, §10.5) — "a single CSPRNG draw, negligible
// relative to the OpenBao write it precedes" (§21).
type SecretGenerator func() (string, error)

// secretBytes is the CSPRNG draw size: 256 bits, in line with a Keycloak
// client-credentials secret.
const secretBytes = 32

// DefaultSecretGenerator draws secretBytes from crypto/rand and encodes them
// base64 URL-safe without padding — a fixed-length, URL/header-safe secret.
func DefaultSecretGenerator() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("service: generate secret material: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
