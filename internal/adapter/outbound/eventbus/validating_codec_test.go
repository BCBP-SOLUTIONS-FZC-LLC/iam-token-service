package eventbus

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

type recordingCodec struct {
	calls      int
	gotType    string
	gotPayload []byte
}

func (r *recordingCodec) Encode(_ context.Context, eventType string, payload json.RawMessage) ([]byte, string, error) {
	r.calls++
	r.gotType = eventType
	r.gotPayload = payload
	return payload, "inner-version", nil
}

func (r *recordingCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return encoded, nil
}

func TestNewValidatingCodec_CompilesAllEmbeddedSchemas(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)
	// 5 frozen produced events (§25) + TenantMembershipsPurged (consumed,
	// kept for schema-gov coverage) — every schemas/*.json file compiles.
	assert.Len(t, c.schemas, 6)
	for _, name := range []string{
		"ServiceAccountRegistered",
		"ServiceAccountCredentialIssued",
		"ServiceAccountCredentialRotated",
		"ServiceAccountCredentialRevoked",
		"ServiceAccountRevoked",
		"TenantMembershipsPurged",
	} {
		assert.Contains(t, c.schemas, name)
	}
}

func TestValidatingCodec_Encode_ValidPayloadPassesThroughToInner(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	payload := []byte(`{
		"tenant_id": "11111111-1111-1111-1111-111111111111",
		"principal_id": "22222222-2222-2222-2222-222222222222",
		"principal_sub": "33333333-3333-3333-3333-333333333333",
		"keycloak_client_id": "platform-automation",
		"principal_type": "platform_automation",
		"actor_id": "00000000-0000-0000-0000-0000000000a1",
		"created_at": "2026-01-01T00:00:00Z"
	}`)

	encoded, schemaVersionID, err := c.Encode(context.Background(), "ServiceAccountRegistered", payload)
	require.NoError(t, err)
	assert.Empty(t, schemaVersionID) // NoopCodec inner
	assert.JSONEq(t, string(payload), string(encoded))
}

// ServiceAccountRegistered's keycloak_client_id accepts exactly the two
// shapes TS-4 accepts (domain.ValidPlatformAutomationClientID, §25 rev
// 1.1) — the tenant-scoped one is every RP-1 trial mint and RP-4 revert
// re-mint, which a const "platform-automation" rejected with a 500.
func TestValidatingCodec_Encode_ServiceAccountRegisteredClientIDShapes(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	for clientID, ok := range map[string]bool{
		"platform-automation": true,
		"platform-automation-11111111-1111-1111-1111-111111111111": true,
		"platform-automation-":           false,
		"platform-automation-not-a-uuid": false,
		"other-client":                   false,
	} {
		payload := []byte(`{
			"tenant_id": "11111111-1111-1111-1111-111111111111",
			"principal_id": "22222222-2222-2222-2222-222222222222",
			"principal_sub": "33333333-3333-3333-3333-333333333333",
			"keycloak_client_id": "` + clientID + `",
			"principal_type": "platform_automation",
			"actor_id": "00000000-0000-0000-0000-0000000000a1",
			"created_at": "2026-01-01T00:00:00Z"
		}`)
		_, _, err := c.Encode(context.Background(), "ServiceAccountRegistered", payload)
		if ok {
			assert.NoError(t, err, clientID)
		} else {
			assert.Error(t, err, clientID)
		}
	}
}

func TestValidatingCodec_Encode_MissingRequiredFieldFails(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	// Missing required "created_at".
	payload := []byte(`{
		"tenant_id": "11111111-1111-1111-1111-111111111111",
		"principal_id": "22222222-2222-2222-2222-222222222222",
		"principal_sub": "33333333-3333-3333-3333-333333333333",
		"keycloak_client_id": "platform-automation",
		"principal_type": "platform_automation"
	}`)

	inner := &recordingCodec{}
	c2 := &ValidatingCodec{inner: inner, schemas: c.schemas}
	_, _, err = c2.Encode(context.Background(), "ServiceAccountRegistered", payload)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate ServiceAccountRegistered")
	assert.Zero(t, inner.calls, "inner codec must not be called when validation fails")
}

func TestValidatingCodec_Encode_NonJSONPayloadFails(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	_, _, err = c.Encode(context.Background(), "ServiceAccountRegistered", []byte(`not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "payload is not JSON")
}

// TestValidatingCodec_Encode_UnregisteredEventTypePassesThroughUnvalidated
// covers the pass-through branch for an event type with no embedded schema
// at all — every event type this service actually publishes or consumes
// has one (all 6 schemas/*.json files compile, TestNewValidatingCodec_
// CompilesAllEmbeddedSchemas above), so this only protects a future caller
// invoking Encode with an unrelated string.
func TestValidatingCodec_Encode_UnregisteredEventTypePassesThroughUnvalidated(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	inner := &recordingCodec{}
	c2 := &ValidatingCodec{inner: inner, schemas: c.schemas}

	payload := []byte(`{"anything":"goes"}`)
	encoded, schemaVersionID, err := c2.Encode(context.Background(), "SomeFutureEventTypeWithNoSchema", payload)
	require.NoError(t, err)
	assert.Equal(t, "inner-version", schemaVersionID)
	assert.Equal(t, payload, encoded)
	assert.Equal(t, 1, inner.calls)
	assert.Equal(t, "SomeFutureEventTypeWithNoSchema", inner.gotType)
}

// TestValidatingCodec_Encode_ConsumedEventSchemaIsAlsoValidated covers
// TenantMembershipsPurged — this service's one *consumed* event
// (api/asyncapi.yaml). Its schema is compiled the same as every produced
// one (NewValidatingCodec compiles every schemas/*.json file), so a call
// with that event type is validated too, even though this service never
// produces it in practice.
func TestValidatingCodec_Encode_ConsumedEventSchemaIsAlsoValidated(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	t.Run("valid payload passes", func(t *testing.T) {
		payload := []byte(`{"tenant_id": "11111111-1111-1111-1111-111111111111"}`)
		_, _, err := c.Encode(context.Background(), "TenantMembershipsPurged", payload)
		require.NoError(t, err)
	})

	t.Run("missing required tenant_id fails", func(t *testing.T) {
		_, _, err := c.Encode(context.Background(), "TenantMembershipsPurged", []byte(`{}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "validate TenantMembershipsPurged")
	})
}

func TestValidatingCodec_Encode_ValidPayloadForEveryProducedEvent(t *testing.T) {
	c, err := NewValidatingCodec(NoopCodec{})
	require.NoError(t, err)

	cases := map[string]string{
		"ServiceAccountRegistered": `{
			"tenant_id": "11111111-1111-1111-1111-111111111111",
			"principal_id": "22222222-2222-2222-2222-222222222222",
			"principal_sub": "33333333-3333-3333-3333-333333333333",
			"keycloak_client_id": "platform-automation",
			"principal_type": "platform_automation",
			"actor_id": "00000000-0000-0000-0000-0000000000a1",
			"created_at": "2026-01-01T00:00:00Z"
		}`,
		"ServiceAccountCredentialIssued": `{
			"tenant_id": "11111111-1111-1111-1111-111111111111",
			"principal_id": "22222222-2222-2222-2222-222222222222",
			"version": 1,
			"actor_id": "00000000-0000-0000-0000-0000000000a1",
			"issued_at": "2026-01-01T00:00:00Z"
		}`,
		"ServiceAccountCredentialRotated": `{
			"tenant_id": "11111111-1111-1111-1111-111111111111",
			"principal_id": "22222222-2222-2222-2222-222222222222",
			"version": 2,
			"prior_version": 1,
			"actor_id": "00000000-0000-0000-0000-0000000000a1",
			"expires_prior_at": "2026-01-01T00:00:00Z"
		}`,
		"ServiceAccountCredentialRevoked": `{
			"tenant_id": "11111111-1111-1111-1111-111111111111",
			"principal_id": "22222222-2222-2222-2222-222222222222",
			"version": 1,
			"actor_id": "00000000-0000-0000-0000-0000000000a1",
			"revoked_at": "2026-01-01T00:00:00Z"
		}`,
		"ServiceAccountRevoked": `{
			"tenant_id": "11111111-1111-1111-1111-111111111111",
			"principal_id": "22222222-2222-2222-2222-222222222222",
			"actor_id": "00000000-0000-0000-0000-0000000000a1",
			"revoked_at": "2026-01-01T00:00:00Z"
		}`,
	}

	for eventType, payload := range cases {
		t.Run(eventType, func(t *testing.T) {
			_, _, err := c.Encode(context.Background(), eventType, []byte(payload))
			require.NoError(t, err)
		})
	}
}

func TestValidatingCodec_Decode_DelegatesToInner(t *testing.T) {
	inner := &recordingCodec{}
	c := &ValidatingCodec{inner: inner}
	got, err := c.Decode(context.Background(), "schema-id", []byte(`{"k":"v"}`))
	require.NoError(t, err)
	assert.Equal(t, json.RawMessage(`{"k":"v"}`), got)
}

func TestNewValidatingCodec_NilInnerBecomesNoop(t *testing.T) {
	c, err := NewValidatingCodec(nil)
	require.NoError(t, err)
	assert.Equal(t, events.NoopCodec{}, c.inner)
}
