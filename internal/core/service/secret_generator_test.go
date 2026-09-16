package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DefaultSecretGenerator's rand.Read failure branch is not exercised here:
// in this Go toolchain, crypto/rand.Read treats a broken entropy source as
// unrecoverable (runtime.fatal, not a normal returned error — confirmed
// empirically; swapping the package-level rand.Reader var does not even
// intercept it), so there is no safe, in-process way to drive that branch.
// It remains defensive dead code from a testing standpoint.

func TestDefaultSecretGenerator_ReturnsURLSafeMaterial(t *testing.T) {
	s1, err := DefaultSecretGenerator()
	require.NoError(t, err)
	require.NotEmpty(t, s1)
	// 32 bytes base64 URL-safe, no padding, encodes to 43 chars.
	assert.Len(t, s1, 43)
	for _, r := range s1 {
		assert.NotContains(t, "+/=", string(r), "must be URL-safe, unpadded base64")
	}

	s2, err := DefaultSecretGenerator()
	require.NoError(t, err)
	assert.NotEqual(t, s1, s2, "two draws should not collide")
}
