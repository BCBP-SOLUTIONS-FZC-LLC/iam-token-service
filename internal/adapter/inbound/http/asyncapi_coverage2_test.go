// Coverage tests closing the remaining asyncapi.go gaps: UnmarshalYAML's
// decode-error branch, AsyncAPIHandler's spec-load-failure branch,
// renderPage's env-color switch cases + long-description truncation,
// renderServers' nil/zero-Kind guard + description truncation,
// renderMessage's title-fallback + non-empty event_type branch, and
// renderPropsTable's property-order-fallback / seen-guard / item-enum /
// example branches.
package http

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// ── UnmarshalYAML — decode error propagates ──────────────────────────────

func TestUnmarshalYAML_DecodeErrorPropagates(t *testing.T) {
	var s asyncSchema
	// "required" must decode into []string; a mapping there is a type
	// mismatch that fails inside value.Decode((*rawSchema)(s)).
	err := yaml.Unmarshal([]byte("required: {a: b}\n"), &s)
	require.Error(t, err)
}

// ── AsyncAPIHandler — spec-load failure renders 500 ───────────────────────

func TestAsyncAPIHandler_SpecLoadErrorReturns500(t *testing.T) {
	// White-box: force the package-level cache into its failure state
	// directly. loadAsyncSpec's sync.Once means the real embedded spec can
	// only ever be parsed once per process, so first ensure it HAS already
	// fired (idempotent — a no-op if some earlier test already triggered
	// it), then overwrite the cached result. Once asyncSpecOnce has fired,
	// asyncSpecOnce.Do is permanently a no-op, so loadAsyncSpec() always
	// returns whatever is in asyncSpecVal/asyncSpecErr from here on —
	// mutating them directly (without touching the Once itself, which
	// go vet's copylocks check forbids copying) is sufficient. Save +
	// restore so subsequent tests still see the successfully-cached spec.
	_, _ = loadAsyncSpec()
	origVal, origErr := asyncSpecVal, asyncSpecErr
	defer func() { asyncSpecVal, asyncSpecErr = origVal, origErr }()

	asyncSpecVal = nil
	asyncSpecErr = errors.New("boom: spec unavailable")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)

	AsyncAPIHandler(c)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "internal_error")
	assert.Contains(t, w.Body.String(), "AsyncAPI spec unavailable")
}

// ── renderPage — env badge color switch + long description truncation ────

func minimalSpec() *asyncSpec {
	return &asyncSpec{
		Info: asyncInfo{Title: "T", Version: "1.0"},
		Comps: asyncComponents{
			Messages: map[string]asyncMessage{},
			Schemas:  map[string]asyncSchema{},
		},
	}
}

func TestRenderPage_ProductionEnv_RedBadge(t *testing.T) {
	var buf bytes.Buffer
	renderPage(&buf, minimalSpec(), "production")
	assert.Contains(t, buf.String(), "#ec4b3c;color:#fff;margin-left:auto")
	assert.Contains(t, buf.String(), "PRODUCTION")
}

func TestRenderPage_StagingEnv_YellowBadge(t *testing.T) {
	var buf bytes.Buffer
	renderPage(&buf, minimalSpec(), "staging")
	assert.Contains(t, buf.String(), "#f59e0b;color:#fff;margin-left:auto")
	assert.Contains(t, buf.String(), "STAGING")
}

func TestRenderPage_LongDescription_Truncated(t *testing.T) {
	spec := minimalSpec()
	spec.Info.Desc = strings.Repeat("x", 900)
	var buf bytes.Buffer
	renderPage(&buf, spec, "dev")
	assert.Contains(t, buf.String(), "[truncated — see api/asyncapi.yaml for full description]")
}

// This also exercises renderServers' `node == nil || node.Kind == 0` guard
// (minimalSpec's zero-value Servers node) since renderPage calls it inline.
func TestRenderPage_NoServers_RendersWithoutServersSection(t *testing.T) {
	var buf bytes.Buffer
	renderPage(&buf, minimalSpec(), "dev")
	assert.NotContains(t, buf.String(), `id="servers"`)
}

// ── renderServers — explicit nil-node guard + description truncation ─────

func TestRenderServers_NilNode_NoOp(t *testing.T) {
	var buf bytes.Buffer
	renderServers(&buf, nil)
	assert.Empty(t, buf.String())
}

func TestRenderServers_LongDescriptionTruncated(t *testing.T) {
	long := strings.Repeat("y", 250)
	var s asyncSpec
	src := "servers:\n  test-server:\n    host: h.example.com\n    protocol: sns\n    description: \"" + long + "\"\n"
	require.NoError(t, yaml.Unmarshal([]byte(src), &s))

	var buf bytes.Buffer
	renderServers(&buf, &s.Servers)

	assert.Contains(t, buf.String(), "…")
	assert.Contains(t, buf.String(), "test-server")
	assert.Contains(t, buf.String(), "SNS") // protocol upper-cased
}

// ── renderMessage — title fallback to name + non-empty event_type badge ──

func TestRenderMessage_TitleFallsBackToName_AndEventTypeBadgeRendered(t *testing.T) {
	src := `
asyncapi: "3.0.0"
info:
  title: T
  version: "1"
components:
  messages:
    FooHappened:
      summary: "Something happened"
      contentType: application/json
      payload:
        $ref: '#/components/schemas/Foo'
      bindings:
        sns:
          messageAttributes:
            EventType:
              value: FooHappened
  schemas:
    Foo:
      type: object
      properties:
        id:
          type: string
`
	spec, err := readAsyncSpec([]byte(src))
	require.NoError(t, err)
	msg := spec.Comps.Messages["FooHappened"]

	var buf bytes.Buffer
	renderMessage(&buf, "FooHappened", &msg, &spec.Comps)

	out := buf.String()
	// No Title set on the message → falls back to the message name.
	assert.Contains(t, out, ">FooHappened<")
	// Non-empty eventType → the sns-attr badge branch renders.
	assert.Contains(t, out, `event_type: FooHappened`)
}

// TestRenderMessage_NoEventType_OmitsBadge covers the eventType=="" branch
// of the inline closure that decides whether to render the sns-attr badge
// (the "" return, as opposed to the Sprintf branch covered above) — this
// service's own asyncapi.yaml never sets an SNS event_type binding, so
// every real message here takes this branch.
func TestRenderMessage_NoEventType_OmitsBadge(t *testing.T) {
	msg := &asyncMessage{Title: "Bare", Summary: "no bindings at all"}
	var buf bytes.Buffer
	renderMessage(&buf, "Bare", msg, &asyncComponents{Schemas: map[string]asyncSchema{}})
	assert.NotContains(t, buf.String(), "sns-attr")
}

// ── renderPropsTable — order-fallback, seen-guard, item-enum, example ────

func TestRenderPropsTable_NoPropertyOrder_FallsBackToSortedKeys(t *testing.T) {
	sc := &asyncSchema{
		Properties: map[string]asyncProp{
			"zeta":  {Type: "string"},
			"alpha": {Type: "string"},
		},
		// PropertyOrder intentionally left empty — not populated via
		// UnmarshalYAML since this schema is hand-built.
	}
	var buf bytes.Buffer
	renderPropsTable(&buf, sc, "X")
	out := buf.String()
	// Both properties still render even without declared order.
	assert.Contains(t, out, "alpha")
	assert.Contains(t, out, "zeta")
}

func TestRenderPropsTable_PropertyMissingFromOrder_StillRendered(t *testing.T) {
	sc := &asyncSchema{
		Properties: map[string]asyncProp{
			"a": {Type: "string"},
			"b": {Type: "string"}, // present in map but absent from PropertyOrder
		},
		PropertyOrder: []string{"a"},
	}
	var buf bytes.Buffer
	renderPropsTable(&buf, sc, "X")
	out := buf.String()
	assert.Contains(t, out, ">a<")
	assert.Contains(t, out, ">b<")
}

func TestRenderPropsTable_ItemEnumAndExample_Rendered(t *testing.T) {
	sc := &asyncSchema{
		Properties: map[string]asyncProp{
			"statuses": {
				Type:  "array",
				Items: &asyncProp{Type: "string", Enum: []string{"active", "revoked"}},
			},
			"name": {
				Type:    "string",
				Example: "platform-automation",
			},
		},
		PropertyOrder: []string{"statuses", "name"},
	}
	var buf bytes.Buffer
	renderPropsTable(&buf, sc, "X")
	out := buf.String()
	assert.Contains(t, out, "active")
	assert.Contains(t, out, "revoked")
	assert.Contains(t, out, "platform-automation")
}

func TestRenderPropsTable_NoProperties_RendersEmptyMessage(t *testing.T) {
	var buf bytes.Buffer
	renderPropsTable(&buf, &asyncSchema{}, "X")
	assert.Contains(t, buf.String(), "No properties.")
}

// ── resolveSchema — allOf merging ─────────────────────────────────────────
//
// This service's own api/asyncapi.yaml never uses allOf (every payload
// schema is a flat object), so this branch isn't exercised by
// TestAsyncAPIHandler_RendersEmbeddedSpec — covered here directly instead.

func TestResolveSchema_AllOfMergesSubSchemaProperties(t *testing.T) {
	comps := &asyncComponents{
		Schemas: map[string]asyncSchema{
			"Envelope": {
				Required:      []string{"id"},
				PropertyOrder: []string{"id"},
				Properties:    map[string]asyncProp{"id": {Type: "string"}},
			},
		},
	}
	sc := &asyncSchema{
		AllOf: []asyncSchema{
			{Ref: "#/components/schemas/Envelope"},
			{
				Required:      []string{"tenant_id"},
				PropertyOrder: []string{"tenant_id"},
				Properties:    map[string]asyncProp{"tenant_id": {Type: "string", Format: "uuid"}},
			},
		},
	}

	merged := resolveSchema(sc, comps)

	assert.ElementsMatch(t, []string{"id", "tenant_id"}, merged.Required)
	assert.Contains(t, merged.Properties, "id")
	assert.Contains(t, merged.Properties, "tenant_id")
	assert.Equal(t, []string{"id", "tenant_id"}, merged.PropertyOrder)
}

// TestResolveSchema_AllOfDuplicatePropertyName_NotDoubleAdded covers the
// `seen` guard in resolveSchema's allOf loop — a property name appearing in
// more than one sub-schema must only appear once in PropertyOrder.
func TestResolveSchema_AllOfDuplicatePropertyName_NotDoubleAdded(t *testing.T) {
	comps := &asyncComponents{}
	sc := &asyncSchema{
		AllOf: []asyncSchema{
			{PropertyOrder: []string{"id"}, Properties: map[string]asyncProp{"id": {Type: "string"}}},
			{PropertyOrder: []string{"id"}, Properties: map[string]asyncProp{"id": {Type: "integer"}}},
		},
	}
	merged := resolveSchema(sc, comps)
	assert.Equal(t, []string{"id"}, merged.PropertyOrder)
}

// TestResolveSchema_RefToUnknownSchema_ReturnsSelf covers the branch where a
// $ref doesn't resolve to any known schema (comps.Schemas lookup misses) —
// resolveSchema falls through and returns sc itself unresolved.
func TestResolveSchema_RefToUnknownSchema_ReturnsSelf(t *testing.T) {
	sc := &asyncSchema{Ref: "#/components/schemas/DoesNotExist"}
	got := resolveSchema(sc, &asyncComponents{Schemas: map[string]asyncSchema{}})
	assert.Same(t, sc, got)
}
