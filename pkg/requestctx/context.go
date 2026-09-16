// Package requestctx carries the validated caller identity extracted from
// trusted gateway headers (x-user-id, x-tenant-id) through the request
// scope. Handlers and services read this struct rather than reaching back
// into gin.Context, keeping core packages framework-free. Unlike the
// sibling Realm Provisioner's requestctx, this service has no role concept
// (TS-INV-4 — it makes no authorization decision) so RequestContext carries
// only the two identities every /api/v1/internal/* route requires (§5.2).
package requestctx

import (
	"context"

	"github.com/google/uuid"
)

type contextKey struct{}

// RequestContext holds the validated caller identity for a single request:
// the reserved system principal (UserID) and the target tenant (TenantID,
// the path :id, validated equal to the x-tenant-id GUC — §5.1).
type RequestContext struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
}

// WithContext returns a new context carrying rc.
func WithContext(ctx context.Context, rc *RequestContext) context.Context {
	return context.WithValue(ctx, contextKey{}, rc)
}

// FromContext retrieves the RequestContext set by the GUC-bridge middleware.
func FromContext(ctx context.Context) (*RequestContext, bool) {
	rc, ok := ctx.Value(contextKey{}).(*RequestContext)
	return rc, ok && rc != nil
}
