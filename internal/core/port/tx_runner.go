package port

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// TxRunner is the seam services use to run business writes and event
// enqueues atomically (EVT-1). The concrete implementation is
// postgres.TxRunner, which opens a pgcommon transaction, attaches it to
// ctx for repositories and EventPublisher.Enqueue, and commits both
// together.
//
// Services depend on this port; they never touch pgx or pgcommon
// directly. Repositories participating in the tx read ctx via
// TxFromContext (through the postgres.withPool helper) so a write inside
// RunInTx joins the same tx.
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// txKey is the context key WithTx/TxFromContext use to carry the active
// pgx.Tx. Lives here (not in adapter/outbound/postgres) so outbound
// adapters outside the postgres package — e.g. eventbus.Publisher.Enqueue
// — can read the tx without importing another outbound adapter, which
// go-arch-lint forbids (adapters_outbound components may not depend on
// each other). Matches iam-user-profile / iam-org-membership.
type txKey struct{}

// WithTx stores the active pgx.Tx in ctx so repository helpers and
// EventPublisher.Enqueue join the same pgcommon transaction.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// TxFromContext retrieves the active pgx.Tx set by WithTx, if any.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}
