//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	consumeradapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/inbound/consumer"
	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

// noopSecretStore stands in for OpenBao in this test — no real OpenBao
// dependency is set up in the testcontainers harness, and this test's
// purpose is to validate the real Postgres cascade (RLS GUC binding +
// hard-delete + outbox emission), not the OpenBao SDK calls already
// covered elsewhere (openbao.Client's own logic is untested by mocks by
// design — it is a thin SDK wrapper).
type noopSecretStore struct{ deleted []string }

func (s *noopSecretStore) Write(context.Context, string, string) error  { return nil }
func (s *noopSecretStore) Read(context.Context, string) (string, error) { return "", nil }
func (s *noopSecretStore) Delete(_ context.Context, path string) error {
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

	principalRepo := pgadapter.NewPrincipalRepository(appPool)
	credentialRepo := pgadapter.NewCredentialRepository(appPool)
	processedRepo := pgadapter.NewProcessedEventsRepository(appPool)
	secrets := &noopSecretStore{}

	enqueueCodec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	require.NoError(t, err)
	outboxPublisher := eventbusadapter.New("iam-token-service", enqueueCodec)
	txRunner := pgadapter.NewTxRunner(appPool, outboxPublisher)

	consumer := consumeradapter.NewOffboardingConsumer(principalRepo, credentialRepo, secrets, processedRepo, txRunner, nil)

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

	seen, err := processedRepo.IsProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, env.ID)
	require.NoError(t, err)
	assert.True(t, seen)
}
