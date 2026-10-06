package service

import (
	"crypto/rand"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingReader is an entropy source that always fails.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy source unavailable") }

// Since Go 1.26 crypto/rsa ignores a caller-supplied random source unless
// GODEBUG=cryptocustomrand=1 (crypto/internal/rand.CustomReader), which is
// why merely swapping rand.Reader does not reach DefaultKeyGenerator's
// error branch on its own. With the setting on, the swapped reader is used
// and its failure surfaces as a returned, wrapped error.
//
// Not parallel: t.Setenv forbids it, and rand.Reader is process-global —
// it is restored before any paused parallel test resumes.
func TestDefaultKeyGenerator_EntropyFailureIsReturned(t *testing.T) {
	t.Setenv("GODEBUG", "cryptocustomrand=1")
	orig := rand.Reader
	rand.Reader = failingReader{}
	t.Cleanup(func() { rand.Reader = orig })

	key, err := DefaultKeyGenerator()
	require.Error(t, err)
	assert.Empty(t, key)
	assert.Contains(t, err.Error(), "service: generate credential keypair")
}
