package consumer_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/inbound/consumer"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

func newTestConsumer(t *testing.T) (*consumer.OffboardingConsumer, *fakePrincipalRepository, *fakeCredentialRepository, *fakeSecretStore, *fakeInbox, *fakeEventPublisher) {
	t.Helper()
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), nil)
	return c, principals, credentials, secrets, processed, pub
}

func tenantMembershipsPurgedEnvelope(tenantID uuid.UUID) events.Envelope[json.RawMessage] {
	payload, _ := json.Marshal(map[string]any{"tenant_id": tenantID.String()})
	return events.NewEnvelope("TenantMembershipsPurged", "core", json.RawMessage(payload))
}

func TestOffboardingConsumer_Handle_DeletesAndEmits(t *testing.T) {
	c, principals, credentials, secrets, processed, pub := newTestConsumer(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := uuid.New()

	principals.seed(tenantID, &domain.ServiceAccountPrincipal{
		ID: principalID, TenantID: tenantID, PrincipalSub: uuid.New(),
		KeycloakClientID: domain.KeycloakClientPlatformAutomation, PrincipalType: domain.PrincipalTypePlatformAutomation,
		Status: domain.PrincipalStatusActive,
	})
	credentials.seed(principalID, &domain.Credential{
		ID: uuid.New(), TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusActive, OpenBaoPath: domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1),
	})

	env := tenantMembershipsPurgedEnvelope(tenantID)
	require.NoError(t, c.Handle(ctx, env))

	assert.Contains(t, principals.deleted, tenantID)
	assert.Empty(t, principals.byTenant[tenantID], "principal rows must be gone")
	require.Len(t, secrets.deleted, 1)
	assert.Equal(t, domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1), secrets.deleted[0])

	require.Len(t, pub.events, 1)
	assert.Equal(t, domain.EventServiceAccountRevoked, pub.events[0].Type)
	revokedPayload, ok := pub.events[0].Data.(domain.ServiceAccountRevokedPayload)
	require.True(t, ok)
	assert.Equal(t, principalID, revokedPayload.PrincipalID)

	seen := processed.IsProcessed(port.ProcessedEventsConsumerTenantOffboarding, env.ID)
	assert.True(t, seen)
}

func TestOffboardingConsumer_Handle_IdempotentRedelivery(t *testing.T) {
	c, principals, _, secrets, processed, pub := newTestConsumer(t)
	ctx := context.Background()
	tenantID := uuid.New()
	env := tenantMembershipsPurgedEnvelope(tenantID)

	processed.MarkProcessed(port.ProcessedEventsConsumerTenantOffboarding, env.ID)
	principals.seed(tenantID, &domain.ServiceAccountPrincipal{ID: uuid.New(), TenantID: tenantID})

	require.NoError(t, c.Handle(ctx, env))

	assert.Empty(t, principals.deleted, "an already-processed event must not touch principals")
	assert.Empty(t, secrets.deleted)
	assert.Empty(t, pub.events, "a duplicate delivery must not re-emit")
}

func TestOffboardingConsumer_Handle_NoPrincipalIsInertNoOp(t *testing.T) {
	c, principals, _, secrets, processed, pub := newTestConsumer(t)
	ctx := context.Background()
	tenantID := uuid.New()
	env := tenantMembershipsPurgedEnvelope(tenantID)

	require.NoError(t, c.Handle(ctx, env))

	assert.Contains(t, principals.deleted, tenantID)
	assert.Empty(t, secrets.deleted)
	assert.Empty(t, pub.events, "no principal found means nothing to revoke")

	seen := processed.IsProcessed(port.ProcessedEventsConsumerTenantOffboarding, env.ID)
	assert.True(t, seen, "even a no-op run must be marked processed so redelivery short-circuits")
}

func TestOffboardingConsumer_Handle_MissingTenantIDIsError(t *testing.T) {
	c, _, _, _, _, _ := newTestConsumer(t)
	ctx := context.Background()

	payload, _ := json.Marshal(map[string]any{})
	env := events.NewEnvelope("TenantMembershipsPurged", "core", json.RawMessage(payload))

	err := c.Handle(ctx, env)
	require.Error(t, err)
}

// A TenantMembershipsPurged that cannot be deduplicated is a permanent
// reject (ErrInvalidEnvelopeID → cmd/consumer's DLQ router), never an ack:
// acking it would silently skip the tenant's GDPR erasure.
func TestOffboardingConsumer_Handle_MissingEnvelopeIDIsRejected(t *testing.T) {
	c, principals, _, secrets, processed, pub := newTestConsumer(t)
	ctx := context.Background()

	payload, _ := json.Marshal(map[string]any{"tenant_id": uuid.New().String()})
	env := events.Envelope[json.RawMessage]{Type: "TenantMembershipsPurged", Source: "core", Payload: json.RawMessage(payload)}

	err := c.Handle(ctx, env)
	require.ErrorIs(t, err, consumer.ErrInvalidEnvelopeID, "a known event without a dedup key must reach the DLQ, not be acked")
	assert.Empty(t, principals.deleted)
	assert.Empty(t, secrets.deleted)
	assert.Empty(t, pub.events)
	assert.Empty(t, processed.marked)
}

func TestOffboardingConsumer_Handle_InvalidEnvelopeIDIsRejected(t *testing.T) {
	c, principals, _, secrets, processed, _ := newTestConsumer(t)
	ctx := context.Background()
	payload, _ := json.Marshal(map[string]any{"tenant_id": uuid.New().String()})
	env := events.Envelope[json.RawMessage]{
		ID: "not-a-uuid", Type: "TenantMembershipsPurged", Source: "core", Payload: json.RawMessage(payload),
	}

	require.ErrorIs(t, c.Handle(ctx, env), consumer.ErrInvalidEnvelopeID)
	assert.Empty(t, principals.deleted)
	assert.Empty(t, secrets.deleted)
	assert.Empty(t, processed.marked)
}

// Forward-compat is unchanged: an UNKNOWN type with a bad id is still acked,
// so a producer schema addition can never DLQ-storm the queue.
func TestOffboardingConsumer_Handle_UnknownTypeWithInvalidEnvelopeIDIsAcked(t *testing.T) {
	c, principals, _, secrets, processed, _ := newTestConsumer(t)
	for _, id := range []string{"", "not-a-uuid"} {
		env := events.Envelope[json.RawMessage]{ID: id, Type: "SomeFutureEvent", Source: "core", Payload: json.RawMessage(`{}`)}
		require.NoError(t, c.Handle(context.Background(), env), "id %q", id)
	}
	assert.Empty(t, principals.deleted)
	assert.Empty(t, secrets.deleted)
	assert.Empty(t, processed.marked)
}

func TestOffboardingConsumer_Handle_UnknownTypeIsAcked(t *testing.T) {
	c, principals, _, secrets, processed, pub := newTestConsumer(t)
	ctx := context.Background()
	env := events.NewEnvelope("SomeFutureEvent", "core", json.RawMessage(`{}`))

	require.NoError(t, c.Handle(ctx, env))
	assert.Empty(t, principals.deleted)
	assert.Empty(t, secrets.deleted)
	assert.Empty(t, pub.events)
	seen := processed.IsProcessed(port.ProcessedEventsConsumerTenantOffboarding, env.ID)
	assert.True(t, seen)
}

func TestOffboardingConsumer_Handle_MalformedPayloadIsError(t *testing.T) {
	c, _, _, _, _, _ := newTestConsumer(t)
	ctx := context.Background()
	env := events.NewEnvelope("TenantMembershipsPurged", "core", json.RawMessage(`not json`))

	err := c.Handle(ctx, env)
	require.Error(t, err)
}

func TestOffboardingConsumer_Handle_InboxClaimError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), nil)
	processed.claimErr = errors.New("db unavailable")

	err := c.Handle(context.Background(), tenantMembershipsPurgedEnvelope(uuid.New()))
	require.EqualError(t, err, "db unavailable")
}

func TestOffboardingConsumer_Handle_ListByTenantError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), nil)
	principals.listByTenantErr = errors.New("db unavailable")

	err := c.Handle(context.Background(), tenantMembershipsPurgedEnvelope(uuid.New()))
	require.EqualError(t, err, "db unavailable")
}

func TestOffboardingConsumer_Handle_ListByPrincipalError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), nil)
	tenantID := uuid.New()
	principals.seed(tenantID, &domain.ServiceAccountPrincipal{ID: uuid.New(), TenantID: tenantID})
	credentials.listByPrincipalErr = errors.New("db unavailable")

	err := c.Handle(context.Background(), tenantMembershipsPurgedEnvelope(tenantID))
	require.EqualError(t, err, "db unavailable")
}

func TestOffboardingConsumer_Handle_SecretDeleteError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), nil)
	tenantID := uuid.New()
	principalID := uuid.New()
	principals.seed(tenantID, &domain.ServiceAccountPrincipal{ID: principalID, TenantID: tenantID})
	credentials.seed(principalID, &domain.Credential{
		ID: uuid.New(), TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusActive, OpenBaoPath: domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1),
	})
	secrets.deleteErr = errors.New("openbao unavailable")

	err := c.Handle(context.Background(), tenantMembershipsPurgedEnvelope(tenantID))
	require.EqualError(t, err, "openbao unavailable")
	assert.Empty(t, principals.deleted, "nothing must be committed in Postgres when the OpenBao delete fails")
}

func TestOffboardingConsumer_Handle_DeleteByTenantError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), nil)
	tenantID := uuid.New()
	principals.seed(tenantID, &domain.ServiceAccountPrincipal{ID: uuid.New(), TenantID: tenantID})
	principals.deleteByTenantErr = errors.New("db unavailable")

	err := c.Handle(context.Background(), tenantMembershipsPurgedEnvelope(tenantID))
	require.EqualError(t, err, "db unavailable")
}

func TestOffboardingConsumer_Handle_InboxCommitError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), nil)
	tenantID := uuid.New()
	principals.seed(tenantID, &domain.ServiceAccountPrincipal{ID: uuid.New(), TenantID: tenantID})
	processed.commitErr = errors.New("db unavailable")
	env := tenantMembershipsPurgedEnvelope(tenantID)

	err := c.Handle(context.Background(), env)
	require.EqualError(t, err, "db unavailable")
	assert.False(t, processed.IsProcessed(port.ProcessedEventsConsumerTenantOffboarding, env.ID),
		"a failed commit leaves the event unclaimed, so the redelivery repeats the cascade")
}

func TestOffboardingConsumer_Handle_NoEventPublisherOnContextStillSucceeds(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	// fakeInbox with events == nil never calls WithEventPublisher, so
	// port.EventPublisherFromContext returns !ok inside the inbox callback —
	// the cascade must still commit, just without an enqueued event.
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed, nil)
	tenantID := uuid.New()
	principals.seed(tenantID, &domain.ServiceAccountPrincipal{ID: uuid.New(), TenantID: tenantID})

	require.NoError(t, c.Handle(context.Background(), tenantMembershipsPurgedEnvelope(tenantID)))
	assert.Contains(t, principals.deleted, tenantID)
}

func TestOffboardingConsumer_Handle_EnqueueError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{enqueueErr: errors.New("outbox unavailable")}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), nil)
	tenantID := uuid.New()
	principals.seed(tenantID, &domain.ServiceAccountPrincipal{ID: uuid.New(), TenantID: tenantID})

	err := c.Handle(context.Background(), tenantMembershipsPurgedEnvelope(tenantID))
	require.EqualError(t, err, "outbox unavailable")
}

// validSpanContext returns a ctx carrying a fixed, valid OTel span context
// so withTraceID's trace_id-stamping branch (offboarding_consumer.go and
// tracing.go) can be exercised deterministically.
func validSpanContext(t *testing.T) (context.Context, trace.TraceID) {
	t.Helper()
	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), sc), traceID
}

func TestOffboardingConsumer_Handle_MissingEnvelopeIDWithLoggerLogsErrorWithTraceID(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	log := &fakeLogger{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), log)

	ctx, traceID := validSpanContext(t)
	payload, _ := json.Marshal(map[string]any{"tenant_id": uuid.New().String()})
	env := events.Envelope[json.RawMessage]{Type: "TenantMembershipsPurged", Source: "core", Payload: json.RawMessage(payload)}

	require.ErrorIs(t, c.Handle(ctx, env), consumer.ErrInvalidEnvelopeID)
	require.Len(t, log.errorCalls, 1)
	require.Len(t, log.errorFields, 1)
	assert.Equal(t, traceID.String(), log.errorFields[0]["trace_id"])
	assert.Empty(t, principals.deleted)
}

func TestOffboardingConsumer_Handle_InvalidEnvelopeIDWithLoggerLogsError(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	log := &fakeLogger{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), log)

	payload, _ := json.Marshal(map[string]any{"tenant_id": uuid.New().String()})
	env := events.Envelope[json.RawMessage]{
		ID: "not-a-uuid", Type: "TenantMembershipsPurged", Source: "core", Payload: json.RawMessage(payload),
	}

	require.ErrorIs(t, c.Handle(context.Background(), env), consumer.ErrInvalidEnvelopeID)
	require.Len(t, log.errorCalls, 1)
	assert.Contains(t, log.errorCalls[0], "tenant erasure has NOT run")
	assert.NotContains(t, log.errorFields[0], "trace_id", "no span in ctx — trace_id must be omitted, not zero-valued")
}

func TestOffboardingConsumer_Handle_UnknownTypeWithLoggerLogsInfo(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	log := &fakeLogger{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), log)
	env := events.NewEnvelope("SomeFutureEvent", "core", json.RawMessage(`{}`))

	require.NoError(t, c.Handle(context.Background(), env))
	require.Len(t, log.infoCalls, 1)
	assert.Contains(t, log.infoCalls[0], "unknown event type")
}

func TestOffboardingConsumer_Handle_UnknownTypeLogCarriesTraceID(t *testing.T) {
	log := &fakeLogger{}
	processed := newFakeInbox()
	c := consumer.NewOffboardingConsumer(newFakePrincipalRepository(), newFakeCredentialRepository(), newFakeSecretStore(),
		processed.publishing(&fakeEventPublisher{}), log)
	ctx, traceID := validSpanContext(t)

	require.NoError(t, c.Handle(ctx, events.NewEnvelope("SomeFutureEvent", "core", json.RawMessage(`{}`))))
	require.Len(t, log.infoFields, 1)
	assert.Equal(t, traceID.String(), log.infoFields[0]["trace_id"])
}

func TestOffboardingConsumer_Handle_IdempotentRedelivery_WithValidSpanLogsTraceID(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	log := &fakeLogger{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), log)

	ctx, traceID := validSpanContext(t)
	tenantID := uuid.New()
	env := tenantMembershipsPurgedEnvelope(tenantID)
	processed.MarkProcessed(port.ProcessedEventsConsumerTenantOffboarding, env.ID)

	require.NoError(t, c.Handle(ctx, env))
	require.Len(t, log.debugFields, 1)
	assert.Equal(t, traceID.String(), log.debugFields[0]["trace_id"])
}

func TestOffboardingConsumer_Handle_IdempotentRedelivery_LogsViaRealLogger(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	secrets := newFakeSecretStore()
	processed := newFakeInbox()
	pub := &fakeEventPublisher{}
	log := &fakeLogger{}
	c := consumer.NewOffboardingConsumer(principals, credentials, secrets, processed.publishing(pub), log)
	ctx := context.Background()
	tenantID := uuid.New()
	env := tenantMembershipsPurgedEnvelope(tenantID)

	processed.MarkProcessed(port.ProcessedEventsConsumerTenantOffboarding, env.ID)

	require.NoError(t, c.Handle(ctx, env))
	require.Len(t, log.debugCalls, 1)
	assert.Contains(t, log.debugCalls[0], "already processed")
}

// Material a failed or racing TS-1 wrote without a committed row is erased
// too: the cascade walks the tenant's whole OpenBao subtree (§15.2).
func TestOffboardingConsumer_Handle_ErasesUncommittedMaterialUnderTenantPrefix(t *testing.T) {
	c, principals, credentials, secrets, _, _ := newTestConsumer(t)
	tenantID, principalID := uuid.New(), uuid.New()
	principals.seed(tenantID, &domain.ServiceAccountPrincipal{ID: principalID, TenantID: tenantID})
	committed := domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 1)
	credentials.seed(principalID, &domain.Credential{
		ID: uuid.New(), TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusActive, OpenBaoPath: committed,
	})
	tenantPrefix := domain.OpenBaoTenantPrefix(tenantID)
	clientPrefix := domain.OpenBaoPathPrefixFor(tenantID, domain.KeycloakClientPlatformAutomation)
	secrets.children = map[string][]string{
		tenantPrefix: {domain.KeycloakClientPlatformAutomation + "/"},
		clientPrefix: {"v1", "v2"}, // v2: written by a TS-1 that never committed
	}

	require.NoError(t, c.Handle(context.Background(), tenantMembershipsPurgedEnvelope(tenantID)))

	assert.Contains(t, secrets.deleted, domain.OpenBaoPathFor(tenantID, domain.KeycloakClientPlatformAutomation, 2),
		"uncommitted material under the tenant prefix must not survive offboarding")
	assert.Contains(t, secrets.deleted, committed)
}
