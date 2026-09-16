// Package contract_test holds this service's event-contract tests (§14.3):
// no external services (Postgres/OpenBao/AWS) required, but distinct from
// test/unit's use-case tests — these assert properties of the wire
// contract itself (the embedded JSON Schemas and the Go payload types),
// not business logic.
package contract_test

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
)

// forbiddenFieldName matches a credential/secret field name — the same
// vocabulary the CI secret-logging gate (.github/scripts/check-no-secret-log.sh)
// uses for TS-INV-2. No published event payload may ever carry a field
// matching this pattern (EVT-2).
var forbiddenFieldName = regexp.MustCompile(`(?i)^(secret|plaintext|client_secret)$`)

// payloadTypes lists every published event's Go payload type (§7.5,
// domain/events.go) — kept in sync manually since Go has no reflection
// over "every type in this package."
var payloadTypes = []any{
	domain.ServiceAccountRegisteredPayload{},
	domain.ServiceAccountCredentialIssuedPayload{},
	domain.ServiceAccountCredentialRotatedPayload{},
	domain.ServiceAccountCredentialRevokedPayload{},
	domain.ServiceAccountRevokedPayload{},
}

// TestNoSecretFieldInPayloadStructs walks every published payload struct's
// `json` tags and asserts none match forbiddenFieldName (EVT-2).
func TestNoSecretFieldInPayloadStructs(t *testing.T) {
	for _, p := range payloadTypes {
		typ := reflect.TypeOf(p)
		t.Run(typ.Name(), func(t *testing.T) {
			for f := range typ.Fields() {
				tag := f.Tag.Get("json")
				name, _, _ := strings.Cut(tag, ",")
				if name == "" {
					name = f.Name
				}
				assert.False(t, forbiddenFieldName.MatchString(name),
					"payload field %q on %s matches a forbidden secret-field name (EVT-2)", name, typ.Name())
			}
		})
	}
}

// TestNoSecretFieldInEventSchemas walks every embedded JSON Schema's
// top-level `properties` keys and asserts none match forbiddenFieldName
// (EVT-2) — the schema-level counterpart to the Go-struct check above, so
// a schema hand-edited independently of the Go type is still caught.
func TestNoSecretFieldInEventSchemas(t *testing.T) {
	schemas, err := eventbus.ProducedSchemas()
	require.NoError(t, err)
	require.NotEmpty(t, schemas, "eventbus.ProducedSchemas() must not be empty")
	for name, raw := range schemas {
		t.Run(name, func(t *testing.T) {
			var doc struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			require.NoError(t, json.Unmarshal(raw, &doc))
			require.NotEmpty(t, doc.Properties, "schema %s has no properties", name)
			for field := range doc.Properties {
				assert.False(t, forbiddenFieldName.MatchString(field),
					"schema %s property %q matches a forbidden secret-field name (EVT-2)", name, field)
			}
		})
	}
}

// TestEventTypeConstantsMatchSchemas asserts the frozen event-type
// constants (domain/events.go, §25) and the produced embedded schema set
// (eventbus.ProducedSchemas) name exactly the same 5 events — a schema
// added/renamed without updating the other would otherwise silently ship
// unvalidated.
func TestEventTypeConstantsMatchSchemas(t *testing.T) {
	constants := []string{
		domain.EventServiceAccountRegistered,
		domain.EventServiceAccountCredentialIssued,
		domain.EventServiceAccountCredentialRotated,
		domain.EventServiceAccountCredentialRevoked,
		domain.EventServiceAccountRevoked,
	}
	schemas, err := eventbus.ProducedSchemas()
	require.NoError(t, err)
	require.Len(t, schemas, len(constants))
	for _, c := range constants {
		_, ok := schemas[c]
		assert.True(t, ok, "no embedded schema for event type constant %q", c)
	}
}
