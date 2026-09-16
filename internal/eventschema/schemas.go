// Package eventschema embeds the JSON Schema documents used to validate
// outbound event payloads before they are written to the transactional
// outbox (internal/adapter/outbound/eventbus/validating_codec.go). One
// schema per published event type (§7.5, §25) — the 5 frozen event names.
// These are payload-only schemas (they describe the shape of the
// envelope's `data` field, not the envelope itself); see api/asyncapi.yaml
// for the full documented contract including the envelope.
package eventschema

import _ "embed"

// ServiceAccountRegistered is the embedded JSON Schema for the
// ServiceAccountRegistered event payload.
//
//go:embed service_account_registered.json
var ServiceAccountRegistered []byte

// ServiceAccountCredentialIssued is the embedded JSON Schema for the
// ServiceAccountCredentialIssued event payload.
//
//go:embed service_account_credential_issued.json
var ServiceAccountCredentialIssued []byte

// ServiceAccountCredentialRotated is the embedded JSON Schema for the
// ServiceAccountCredentialRotated event payload.
//
//go:embed service_account_credential_rotated.json
var ServiceAccountCredentialRotated []byte

// ServiceAccountCredentialRevoked is the embedded JSON Schema for the
// ServiceAccountCredentialRevoked event payload.
//
//go:embed service_account_credential_revoked.json
var ServiceAccountCredentialRevoked []byte

// ServiceAccountRevoked is the embedded JSON Schema for the
// ServiceAccountRevoked event payload.
//
//go:embed service_account_revoked.json
var ServiceAccountRevoked []byte

// ByEventType maps the frozen PascalCase event type name (§25) to its
// embedded schema, for the validating codec to compile at startup.
var ByEventType = map[string][]byte{
	"ServiceAccountRegistered":        ServiceAccountRegistered,
	"ServiceAccountCredentialIssued":  ServiceAccountCredentialIssued,
	"ServiceAccountCredentialRotated": ServiceAccountCredentialRotated,
	"ServiceAccountCredentialRevoked": ServiceAccountCredentialRevoked,
	"ServiceAccountRevoked":           ServiceAccountRevoked,
}
