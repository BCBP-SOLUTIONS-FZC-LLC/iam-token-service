package openbao

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
)

// kvFor clones the SDK client per call; the clone re-reads the BAO_*
// environment, so an environment that turned invalid after startup fails
// the clone. That surfaces as the same 502 secret_store_unavailable as any
// other OpenBao failure (op "clone_client"), after a successful login, and
// no KV request is attempted.
func TestWithKV_CloneFailureIsSecretStoreUnavailable(t *testing.T) {
	srv := newFakeBaoServer(t)
	c := newTestClient(t, srv.srv.URL)
	require.NoError(t, c.Health(t.Context()), "login works before the environment changes")

	t.Setenv("BAO_MAX_RETRIES", "not-a-number")

	err := c.Write(t.Context(), "iam/serviceaccount/write", "s3cr3t")
	require.Error(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
	assert.Contains(t, de.Message, "clone_client")
	assert.Equal(t, int32(1), srv.loginCalls.Load(), "the cached token was reused; only the clone failed")
}
