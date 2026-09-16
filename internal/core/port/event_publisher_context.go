package port

import "context"

type contextPublisherKey struct{}

// EventPublisherFromContext retrieves the publisher injected by
// TxRunner.RunInTx for the duration of the active transaction.
func EventPublisherFromContext(ctx context.Context) (EventPublisher, bool) {
	p, ok := ctx.Value(contextPublisherKey{}).(EventPublisher)
	return p, ok
}

// WithEventPublisher stores a publisher in ctx. Production TxRunner injects
// the eventbus Publisher after attaching the running tx; tests inject fakes.
func WithEventPublisher(ctx context.Context, p EventPublisher) context.Context {
	return context.WithValue(ctx, contextPublisherKey{}, p)
}
