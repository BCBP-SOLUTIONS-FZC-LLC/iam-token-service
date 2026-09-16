package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/glue"
	gluetypes "github.com/aws/aws-sdk-go-v2/service/glue/types"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
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
type GlueCodec struct {
	client       *glue.Client
	registryName string
	mu           sync.RWMutex
	versionCache map[string]string // schema name -> schema version UUID string
	log          port.Logger
}

var _ events.Codec = (*GlueCodec)(nil)

// NewGlueCodec prefetches the current version ID of every name in
// schemaNames from registryName. Failure here is fatal at startup (the pod
// crashes and Kubernetes restarts it) — a pod that cannot resolve its
// schema versions would otherwise silently publish malformed events.
func NewGlueCodec(ctx context.Context, client *glue.Client, registryName string, schemaNames []string) (*GlueCodec, error) {
	c := &GlueCodec{client: client, registryName: registryName, versionCache: make(map[string]string, len(schemaNames))}
	for _, name := range schemaNames {
		id, err := c.fetchVersionID(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("prefetch glue schema %q in registry %q: %w", name, registryName, err)
		}
		c.versionCache[name] = id
	}
	return c, nil
}

// WithLogger attaches a logger for background refresh warnings and returns
// the codec for chaining.
func (g *GlueCodec) WithLogger(log port.Logger) *GlueCodec {
	g.log = log
	return g
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
// payload. Not used by this service today (no producer this service consumes
// carries a Glue header it needs to strip via this codec, §7.1 — the
// TenantMembershipsPurged consumer lands in a later milestone with its own
// decode path), kept so GlueCodec satisfies events.Codec symmetrically.
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
	out, err := g.client.GetSchemaVersion(ctx, &glue.GetSchemaVersionInput{
		SchemaId:            &gluetypes.SchemaId{SchemaName: &schemaName, RegistryName: &g.registryName},
		SchemaVersionNumber: &gluetypes.SchemaVersionNumber{LatestVersion: true},
	})
	if err != nil {
		return "", err
	}
	if out.SchemaVersionId == nil {
		return "", fmt.Errorf("nil SchemaVersionId for schema %q in registry %q", schemaName, g.registryName)
	}
	return *out.SchemaVersionId, nil
}

// StartRefresher re-fetches every cached schema version ID on an interval
// (background goroutine, exits on ctx.Done()); fetch errors are non-fatal
// and logged — a new Glue schema version takes effect without a pod
// restart.
func (g *GlueCodec) StartRefresher(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.mu.RLock()
				names := make([]string, 0, len(g.versionCache))
				for name := range g.versionCache {
					names = append(names, name)
				}
				g.mu.RUnlock()
				for _, name := range names {
					id, err := g.fetchVersionID(ctx, name)
					if err != nil {
						g.warn(ctx, "glue codec refresh failed", name, err)
						continue
					}
					g.mu.Lock()
					g.versionCache[name] = id
					g.mu.Unlock()
				}
			}
		}
	}()
}

func (g *GlueCodec) warn(ctx context.Context, msg, schema string, err error) {
	if g.log == nil {
		return
	}
	fields := map[string]interface{}{"schema": schema, "error": err.Error()}
	if spanCtx := trace.SpanFromContext(ctx).SpanContext(); spanCtx.IsValid() {
		fields["trace_id"] = spanCtx.TraceID().String()
	}
	g.log.Warn(msg, fields)
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
