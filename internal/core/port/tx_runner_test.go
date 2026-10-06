package port_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// stubTx satisfies pgx.Tx; no method is ever called.
type stubTx struct{ pgx.Tx }

func TestTxFromContext_EmptyContextHasNoTx(t *testing.T) {
	t.Parallel()
	tx, ok := port.TxFromContext(context.Background())
	assert.False(t, ok)
	assert.Nil(t, tx)
}

func TestWithTx_RoundTripsTheSameTx(t *testing.T) {
	t.Parallel()
	want := &stubTx{}
	got, ok := port.TxFromContext(port.WithTx(context.Background(), want))
	assert.True(t, ok)
	assert.Same(t, want, got)
}
