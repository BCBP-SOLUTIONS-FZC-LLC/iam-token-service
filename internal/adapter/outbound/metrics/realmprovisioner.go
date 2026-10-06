package metrics

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// InstrumentedRealmProvisionerClient wraps a port.RealmProvisionerClient
// and records every RP-17 call on
// platform_dependency_request_seconds{dependency="realm_provisioner",operation="refresh_keys",outcome}.
// RP-17 failures otherwise show up only folded into the CronJobs' result
// counters, which cannot say whether RP itself is slow or down.
type InstrumentedRealmProvisionerClient struct {
	inner port.RealmProvisionerClient
}

// NewInstrumentedRealmProvisionerClient wraps inner with call instrumentation.
func NewInstrumentedRealmProvisionerClient(inner port.RealmProvisionerClient) *InstrumentedRealmProvisionerClient {
	return &InstrumentedRealmProvisionerClient{inner: inner}
}

var _ port.RealmProvisionerClient = (*InstrumentedRealmProvisionerClient)(nil)

// RefreshKeys delegates to inner, recording the call (retries included).
func (c *InstrumentedRealmProvisionerClient) RefreshKeys(ctx context.Context, tenantID uuid.UUID) error {
	start := time.Now()
	err := c.inner.RefreshKeys(ctx, tenantID)
	observeDependency(DependencyRealmProvisioner, OperationRefreshKeys, err, time.Since(start).Seconds())
	return err
}
