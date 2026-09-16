package eventbus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoopCodec_Encode_PassesPayloadThroughUnchanged(t *testing.T) {
	payload := []byte(`{"a":1}`)
	encoded, schemaVersionID, err := NoopCodec{}.Encode(context.Background(), "SomeEvent", payload)
	require.NoError(t, err)
	assert.Equal(t, payload, encoded)
	assert.Empty(t, schemaVersionID)
}

var _ Codec = NoopCodec{}
