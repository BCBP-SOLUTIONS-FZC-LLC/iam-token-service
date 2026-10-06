package eventbus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

func TestNoopCodec_IsPlatformEventsNoop(t *testing.T) {
	payload := []byte(`{"a":1}`)
	encoded, schemaVersionID, err := NoopCodec{}.Encode(context.Background(), "SomeEvent", payload)
	require.NoError(t, err)
	assert.Equal(t, payload, encoded)
	assert.Empty(t, schemaVersionID)

	decoded, err := NoopCodec{}.Decode(context.Background(), "", encoded)
	require.NoError(t, err)
	assert.Equal(t, payload, []byte(decoded))
}

var (
	_ Codec        = NoopCodec{}
	_ events.Codec = NoopCodec{}
	_ events.Codec = events.NoopCodec{}
)
