//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/inbound/consumer"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// noopSecretStore stands in for OpenBao in this test — no real OpenBao
// dependency is set up in the testcontainers harness, and this test's
// purpose is to validate the real Postgres cascade (RLS GUC binding +
// hard-delete + outbox emission), not the OpenBao SDK calls already
// covered elsewhere (openbao.Client's own logic is untested by mocks by
// design — it is a thin SDK wrapper).
type noopSecretStore struct {
	mu        sync.Mutex
	deleted   []string
	deleteErr error
}

func (s *noopSecretStore) Write(context.Context, string, string) error  { return nil }
func (s *noopSecretStore) Read(context.Context, string) (string, error) { return "", nil }
func (s *noopSecretStore) Delete(_ context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, path)
	return nil
}
func (s *noopSecretStore) List(context.Context, string) ([]string, error) { return nil, nil }

var _ port.SecretStore = (*noopSecretStore)(nil)

// TestOffboardingConsumer_Integration_DeletesAndEmits exercises the real
// wiring cmd/consumer uses in production (RLS-scoped repos, the real
// eventbus.Publisher, a real outbox.Enqueue write) against a real
// Postgres, proving the RLS GUC binding the consumer sets up itself
// (rather than inheriting from an HTTP middleware) actually works, and
// that the offboarding cascade's DB effects and outbox emission are real
// end-to-end (§8.4).
func TestOffboardingConsumer_Integration_DeletesAndEmits(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	seedCredential(t, ctx, rawPool, tenantID, principalID, 1, "active")

	secrets := &noopSecretStore{}
	consumer := newIntegrationConsumer(t, appPool, secrets)

	payload, err := json.Marshal(map[string]any{"tenant_id": tenantID.String()})
	require.NoError(t, err)
	env := events.NewEnvelope("TenantMembershipsPurged", "core", json.RawMessage(payload))

	require.NoError(t, consumer.Handle(ctx, env))

	require.Len(t, secrets.deleted, 1)
	assert.Equal(t, domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 1), secrets.deleted[0])

	var principalCount int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM service_account_principals WHERE tenant_id = $1`, tenantID).Scan(&principalCount))
	assert.Equal(t, 0, principalCount, "principal row must be hard-deleted")

	var credentialCount int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM service_account_credentials WHERE tenant_id = $1`, tenantID).Scan(&credentialCount))
	assert.Equal(t, 0, credentialCount, "credential rows must cascade-delete")

	var eventType string
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT event_type FROM outbox_events WHERE tenant_id = $1`, tenantID.String()).Scan(&eventType))
	assert.Equal(t, domain.EventServiceAccountRevoked, eventType)

	assert.True(t, processedEventRecorded(t, rawPool, env.ID))

	// A redelivery is a duplicate: no second OpenBao delete, no second event.
	require.NoError(t, consumer.Handle(ctx, env))
	assert.Len(t, secrets.deleted, 1, "a duplicate delivery must not repeat the cascade")
	var outboxCount int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenantID.String()).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount)
}

// newIntegrationConsumer wires the consumer exactly as cmd/consumer does:
// RLS-scoped repositories and platform-events' inbox on the app pool, with
// the real outbox publisher bound into the inbox transaction.
func newIntegrationConsumer(t *testing.T, appPool *pgcommon.Pool, secrets port.SecretStore) *consumeradapter.OffboardingConsumer {
	t.Helper()
	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	require.NoError(t, err)
	outboxPublisher := eventbusadapter.New("iam-token-service", enqueueCodec)
	return consumeradapter.NewOffboardingConsumer(
		pgadapter.NewPrincipalRepository(appPool), pgadapter.NewCredentialRepository(appPool), secrets,
		pgadapter.NewInboxRepository(appPool, outboxPublisher), nil)
}

func processedEventRecorded(t *testing.T, rawPool *dbseed.Pool, eventID string) bool {
	t.Helper()
	var n int
	require.NoError(t, rawPool.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_events WHERE event_id = $1 AND consumer = $2`,
		eventID, string(port.ProcessedEventsConsumerTenantOffboarding)).Scan(&n))
	return n == 1
}

func purgedEnvelope(t *testing.T, tenantID uuid.UUID) events.Envelope[json.RawMessage] {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"tenant_id": tenantID.String()})
	require.NoError(t, err)
	return events.NewEnvelope("TenantMembershipsPurged", "core", json.RawMessage(payload))
}

// TestOffboardingConsumer_Integration_FailedCascadeLeavesEventUnclaimed: an
// OpenBao failure inside the inbox transaction rolls the processed_events
// claim back with everything else, so the redelivery runs the cascade.
func TestOffboardingConsumer_Integration_FailedCascadeLeavesEventUnclaimed(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	seedCredential(t, ctx, rawPool, tenantID, principalID, 1, "active")
	secrets := &noopSecretStore{deleteErr: errors.New("openbao unavailable")}
	consumer := newIntegrationConsumer(t, appPool, secrets)
	env := purgedEnvelope(t, tenantID)

	require.Error(t, consumer.Handle(ctx, env))
	assert.False(t, processedEventRecorded(t, rawPool, env.ID), "the claim must roll back with the failed cascade")
	var principalCount int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM service_account_principals WHERE tenant_id = $1`, tenantID).Scan(&principalCount))
	assert.Equal(t, 1, principalCount, "nothing is deleted in Postgres when OpenBao fails (material-first)")

	secrets.mu.Lock()
	secrets.deleteErr = nil
	secrets.mu.Unlock()
	require.NoError(t, consumer.Handle(ctx, env), "the redelivery runs the cascade")
	assert.True(t, processedEventRecorded(t, rawPool, env.ID))
	assert.Len(t, secrets.deleted, 1)
}

// TestOffboardingConsumer_Integration_ConcurrentDeliveriesRunOnce: two
// copies of the same message handled at once (SQS at-least-once, consumer
// concurrency > 1). The inbox claim's row lock makes the second wait and
// then see a duplicate, so the cascade and its events happen once.
func TestOffboardingConsumer_Integration_ConcurrentDeliveriesRunOnce(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	seedCredential(t, ctx, rawPool, tenantID, principalID, 1, "active")
	secrets := &noopSecretStore{}
	consumer := newIntegrationConsumer(t, appPool, secrets)
	env := purgedEnvelope(t, tenantID)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = consumer.Handle(ctx, env)
		}()
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])

	assert.Len(t, secrets.deleted, 1, "the cascade must run once")
	var outboxCount int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, tenantID.String()).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount, "ServiceAccountRevoked is emitted once")
}
