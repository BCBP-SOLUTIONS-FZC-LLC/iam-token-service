package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ValidatingCodec wraps inner (always NoopCodec at enqueue time) and
// validates each event's payload against the embedded JSON Schema for its
// event type before allowing the outbox insert to proceed (§7.3.1).
// Compiles every schemas/*.json file (produced and consumed) once at
// construction time.
type ValidatingCodec struct {
	inner   Codec
	schemas map[string]*jsonschema.Schema
	mu      sync.RWMutex
}

// NewValidatingCodec compiles every embedded schemas/*.json file and
// returns a codec that validates against them, falling through to inner.
// Compilation failure is a build-time programming error surfaced
// immediately (the caller panics on startup — CrashLoopBackoff rather than
// silently publishing malformed events, matching the sibling Realm
// Provisioner convention).
func NewValidatingCodec(inner Codec) (*ValidatingCodec, error) {
	return newValidatingCodecFromFS(inner, schemasFS)
}

// newValidatingCodecFromFS is the testable implementation of
// NewValidatingCodec. It accepts an fs.FS so tests can inject a
// fstest.MapFS to trigger each error branch (ReadDir error, non-.json
// continue, json.Unmarshal error, compile error).
func newValidatingCodecFromFS(inner Codec, schemas fs.FS) (*ValidatingCodec, error) {
	entries, err := fs.ReadDir(schemas, "schemas")
	if err != nil {
		return nil, fmt.Errorf("eventbus: read embedded schemas dir: %w", err)
	}
	c := &ValidatingCodec{inner: inner, schemas: make(map[string]*jsonschema.Schema, len(entries))}
	compiler := jsonschema.NewCompiler()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := fs.ReadFile(schemas, path.Join("schemas", e.Name()))
		if rerr != nil {
			return nil, fmt.Errorf("eventbus: read embedded schema %s: %w", e.Name(), rerr)
		}
		name := eventTypeFromSchemaFile(e.Name())
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("eventbus: parse embedded schema %s: %w", name, err)
		}
		if err := compiler.AddResource(name, doc); err != nil {
			return nil, fmt.Errorf("eventbus: add schema resource %s: %w", name, err)
		}
		sch, err := compiler.Compile(name)
		if err != nil {
			return nil, fmt.Errorf("eventbus: compile schema %s: %w", name, err)
		}
		c.schemas[name] = sch
	}
	return c, nil
}

// Encode validates payload against eventType's schema and then delegates to
// inner. An eventType with no compiled schema at all (neither produced nor
// consumed — i.e. not one of the files under schemas/) passes through
// unvalidated rather than failing; every event type this service actually
// publishes or consumes has an embedded schema, so this branch only
// protects a future caller invoking Encode with an unrelated string.
func (c *ValidatingCodec) Encode(ctx context.Context, eventType string, payload []byte) (encoded []byte, schemaVersionID string, err error) {
	c.mu.RLock()
	sch, ok := c.schemas[eventType]
	c.mu.RUnlock()
	if ok {
		var doc any
		if err := json.Unmarshal(payload, &doc); err != nil {
			return nil, "", fmt.Errorf("validate %s: payload is not JSON: %w", eventType, err)
		}
		if err := sch.Validate(doc); err != nil {
			return nil, "", fmt.Errorf("validate %s: %w", eventType, err)
		}
	}
	return c.inner.Encode(ctx, eventType, payload)
}
