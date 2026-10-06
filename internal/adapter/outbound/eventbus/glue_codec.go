package eventbus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
)

const (
	glueHeaderVersion byte = 0x03 // magic byte for the AWS Glue Schema Registry wire format
	glueNoCompression byte = 0x00
	glueHeaderSize         = 18 // 1 (version) + 1 (compression) + 16 (schema version UUID)
)

// GlueCodec implements events.Codec against the `iam-serviceaccount-events`
// Glue registry (§7.3.1, frozen §25) — this service registers its 5 schema
// names there, one registry with a single producer (unlike the sibling Realm
// Provisioner's shared two-producer registry).
//
// Version resolution is by content, not recency: each schema's version
// UUID is looked up with GetSchemaByDefinition using this binary's own
// embedded schema (ProducedSchemas), so every event is stamped with the
// version this binary actually produces — never merely the registry's
// latest, which runs ahead of the running code when a schema is registered
// before deploy (or after a rollback) and behind it when a deploy races
// schema-registry.yml. The UUID is fixed for the life of the process;
// there is no background refresh.
type GlueCodec struct {
	client       *glue.Client
	registryName string
	definitions  map[string][]byte // schema name -> embedded JSON Schema
	mu           sync.RWMutex
	versionCache map[string]string // schema name -> schema version UUID string
}

var _ events.Codec = (*GlueCodec)(nil)

// NewGlueCodec resolves, for every name in schemaNames, the version in
// registryName whose definition matches this binary's embedded schema.
// Failure here is fatal at startup (the pod crashes and Kubernetes
// restarts it) — including a definition not registered yet, i.e. a deploy
// that outran schema-registry.yml, which self-heals on the first restart
// after registration lands. Starting anyway would stamp events with a
// version that doesn't describe them.
func NewGlueCodec(ctx context.Context, client *glue.Client, registryName string, schemaNames []string) (*GlueCodec, error) {
	definitions, err := ProducedSchemas()
	if err != nil {
		return nil, err
	}
	c := &GlueCodec{client: client, registryName: registryName, definitions: definitions, versionCache: make(map[string]string, len(schemaNames))}
	for _, name := range schemaNames {
		id, err := c.fetchVersionID(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("resolve glue schema %q in registry %q by definition: %w — this binary's schema version isn't registered yet: wait for schema-registry.yml to register it, or run `make schema-verify`", name, registryName, err)
		}
		c.versionCache[name] = id
	}
	return c, nil
}

// Encode prepends the Glue Schema Registry wire header (schema version UUID
// resolved from the cache) to payload.
func (g *GlueCodec) Encode(ctx context.Context, schemaName string, payload json.RawMessage) (encoded []byte, schemaVersionID string, err error) {
	versionID, err := g.versionID(ctx, schemaName)
	if err != nil {
		return nil, "", fmt.Errorf("get glue schema version %q: %w", schemaName, err)
	}
	out, err := prependGlueHeader(versionID, payload)
	if err != nil {
		return nil, "", err
	}
	return out, versionID, nil
}

// Decode strips the Glue wire header from encoded, returning the raw JSON
// payload. Kept so GlueCodec satisfies events.Codec symmetrically; the
// inbound consumer uses the registry-free GlueDecoder instead.
func (g *GlueCodec) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return stripGlueHeader(encoded)
}

func (g *GlueCodec) versionID(ctx context.Context, schemaName string) (string, error) {
	g.mu.RLock()
	id, ok := g.versionCache[schemaName]
	g.mu.RUnlock()
	if ok {
		return id, nil
	}
	id, err := g.fetchVersionID(ctx, schemaName)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.versionCache[schemaName] = id
	g.mu.Unlock()
	return id, nil
}

func (g *GlueCodec) fetchVersionID(ctx context.Context, schemaName string) (string, error) {
	raw, ok := g.definitions[schemaName]
	if !ok {
		return "", fmt.Errorf("no embedded schema for %q", schemaName)
	}
	definition, err := registeredDefinition(raw)
	if err != nil {
		return "", fmt.Errorf("schema %q: %w", schemaName, err)
	}
	out, err := g.client.GetSchemaByDefinition(ctx, &glue.GetSchemaByDefinitionInput{
		SchemaId:         &gluetypes.SchemaId{SchemaName: &schemaName, RegistryName: &g.registryName},
		SchemaDefinition: &definition,
	})
	if err != nil {
		return "", err
	}
	if out.SchemaVersionId == nil {
		return "", fmt.Errorf("nil SchemaVersionId for schema %q in registry %q", schemaName, g.registryName)
	}
	if out.Status != gluetypes.SchemaVersionStatusAvailable {
		return "", fmt.Errorf("schema %q version %s in registry %q is %s, not AVAILABLE", schemaName, *out.SchemaVersionId, g.registryName, out.Status)
	}
	return *out.SchemaVersionId, nil
}

// registeredDefinition returns raw in exactly the form schema-gov register
// uploads it: Python's json.dumps(schema, separators=(",", ":")) —
// compact, key order preserved, non-ASCII escaped as \uXXXX (ensure_ascii
// defaults to True). Sending the byte-identical string makes
// GetSchemaByDefinition match whether or not Glue normalises JSON
// definitions. TestRegisteredDefinition_MatchesSchemaGov pins this against
// real Python for every embedded schema.
func registeredDefinition(raw []byte) (string, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return "", fmt.Errorf("compact schema: %w", err)
	}
	return asciiEscape(compact.Bytes()), nil
}

// asciiEscape rewrites every non-ASCII rune as a JSON \uXXXX escape
// (UTF-16 surrogate pairs above U+FFFF), matching Python's ensure_ascii.
// Non-ASCII can only appear inside JSON strings, so this is always safe.
func asciiEscape(b []byte) string {
	var out strings.Builder
	out.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		switch {
		case r < utf8.RuneSelf:
			out.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&out, "\\u%04x", r)
		}
	}
	return out.String()
}

// GlueDecoder is the consumer-side events.Codec, injected on cmd/consumer's
// SQS consumer via events.WithConsumerCodec. iam-org-membership publishes
// TenantMembershipsPurged through its own GlueCodec (its
// iam-membership-events registry, which this service never reads) — and
// doesn't need to, because Decode only strips the self-describing 18-byte
// header. So unlike GlueCodec this needs no Glue client or registry name.
// platform-events only calls Decode when envelope.dataschema is non-empty,
// so plain-JSON producers (NoopCodec, dev/test) bypass it entirely.
type GlueDecoder struct{}

var _ events.Codec = GlueDecoder{}

// Encode always fails — GlueDecoder is never wired on a publisher.
func (GlueDecoder) Encode(_ context.Context, eventType string, _ json.RawMessage) (encoded []byte, schemaVersionID string, err error) {
	return nil, "", fmt.Errorf("glue decoder: Encode(%q) called on a decode-only codec — use GlueCodec to publish", eventType)
}

// Decode strips the 18-byte Glue wire-format header, same as GlueCodec.Decode.
func (GlueDecoder) Decode(_ context.Context, _ string, encoded []byte) (json.RawMessage, error) {
	return stripGlueHeader(encoded)
}

func prependGlueHeader(schemaVersionID string, payload []byte) ([]byte, error) {
	id, err := uuid.Parse(schemaVersionID)
	if err != nil {
		return nil, fmt.Errorf("parse schema version UUID %q: %w", schemaVersionID, err)
	}
	out := make([]byte, glueHeaderSize+len(payload))
	out[0] = glueHeaderVersion
	out[1] = glueNoCompression
	copy(out[2:18], id[:])
	copy(out[glueHeaderSize:], payload)
	return out, nil
}

func stripGlueHeader(encoded []byte) (json.RawMessage, error) {
	if len(encoded) < glueHeaderSize {
		return nil, fmt.Errorf("glue codec: encoded payload is %d bytes — shorter than the %d-byte Glue header", len(encoded), glueHeaderSize)
	}
	if encoded[0] != glueHeaderVersion {
		return nil, fmt.Errorf("glue codec: unexpected header version byte 0x%02x — want 0x%02x", encoded[0], glueHeaderVersion)
	}
	return json.RawMessage(encoded[glueHeaderSize:]), nil
}
