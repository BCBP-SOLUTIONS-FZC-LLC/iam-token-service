package service

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DefaultKeyGenerator's rand.Read failure branch is not exercised here:
// in this Go toolchain, crypto/rand.Read treats a broken entropy source as
// unrecoverable (runtime.fatal, not a normal returned error — confirmed
// empirically; swapping the package-level rand.Reader var does not even
// intercept it), so there is no safe, in-process way to drive that branch.
// It remains defensive dead code from a testing standpoint.

func TestDefaultKeyGenerator_ReturnsPEMEncodedRSAPrivateKey(t *testing.T) {
	s1, err := DefaultKeyGenerator()
	require.NoError(t, err)
	require.NotEmpty(t, s1)

	block, rest := pem.Decode([]byte(s1))
	require.NotNil(t, block, "must be PEM-encoded")
	assert.Equal(t, "RSA PRIVATE KEY", block.Type)
	assert.Empty(t, rest)

	priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	require.NoError(t, err, "must be a PKCS1 RSA private key")
	assert.Equal(t, keyBits, priv.N.BitLen())

	s2, err := DefaultKeyGenerator()
	require.NoError(t, err)
	assert.NotEqual(t, s1, s2, "two draws should not collide")
}
