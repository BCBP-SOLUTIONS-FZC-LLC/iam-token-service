// asyncapi renderer edge coverage:
//   - propType — array<T>, array<$ref>, format-suffixed types, plain string types
//   - typeHTML — same variants plus $ref → schema anchor
//   - snsEventType — PascalCase key vs snake_case fallback, missing bindings
//   - walkYAML — mapping node walk vs missing / non-mapping node
//   - UnmarshalYAML property-order preservation
package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// propType ────────────────────────────────────────────────────────────

func TestAsyncEdges_PropType_PlainType(t *testing.T) {
	assert.Equal(t, "string", propType(&asyncProp{Type: "string"}))
}

func TestAsyncEdges_PropType_WithFormatSuffix(t *testing.T) {
	assert.Equal(t, "string(uuid)", propType(&asyncProp{Type: "string", Format: "uuid"}))
}

func TestAsyncEdges_PropType_ArrayOfPrimitive(t *testing.T) {
	assert.Equal(t, "array<string>", propType(&asyncProp{Type: "array", Items: &asyncProp{Type: "string"}}))
}

func TestAsyncEdges_PropType_ArrayOfRef(t *testing.T) {
	got := propType(&asyncProp{Type: "array", Items: &asyncProp{Ref: "#/components/schemas/Payload"}})
	assert.Equal(t, "array<Payload>", got)
}

func TestAsyncEdges_PropType_RefDrivesResult(t *testing.T) {
	assert.Equal(t, "Payload", propType(&asyncProp{Ref: "#/components/schemas/Payload"}))
}

// typeHTML ────────────────────────────────────────────────────────────

func TestAsyncEdges_TypeHTML_Ref_HasSchemaAnchor(t *testing.T) {
	got := typeHTML(&asyncProp{Ref: "#/components/schemas/Payload"})
	assert.Contains(t, got, `href="#schema-Payload"`)
	assert.Contains(t, got, `Payload`)
}

func TestAsyncEdges_TypeHTML_ArrayOfRef_HasSchemaAnchor(t *testing.T) {
	got := typeHTML(&asyncProp{Type: "array", Items: &asyncProp{Ref: "#/components/schemas/Payload"}})
	assert.Contains(t, got, "array")
	assert.Contains(t, got, `href="#schema-Payload"`)
}

func TestAsyncEdges_TypeHTML_PlainString(t *testing.T) {
	got := typeHTML(&asyncProp{Type: "string", Format: "uuid"})
	assert.Contains(t, got, "string(uuid)")
}

// walkYAML ────────────────────────────────────────────────────────────

func TestAsyncEdges_WalkYAML_MissingKey_ReturnsEmpty(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("a: 1\nb: 2\n"), &root))
	assert.Equal(t, "", walkYAML(&root, "nonexistent"))
}

func TestAsyncEdges_WalkYAML_NestedMapping_ReturnsValue(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("outer:\n  inner: value42\n"), &root))
	assert.Equal(t, "value42", walkYAML(&root, "outer", "inner"))
}

func TestAsyncEdges_WalkYAML_NilNode_ReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", walkYAML(nil, "x"))
}

func TestAsyncEdges_WalkYAML_EmptyKeys_ReturnsEmpty(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("a: b\n"), &root))
	assert.Equal(t, "", walkYAML(&root))
}

// snsEventType ────────────────────────────────────────────────────────

func TestAsyncEdges_SnsEventType_PascalCasePreferred(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes:
    EventType:
      value: ServiceAccountRegistered
`), &root))
	assert.Equal(t, "ServiceAccountRegistered", snsEventType(&root))
}

func TestAsyncEdges_SnsEventType_FallsBackToSnakeCase(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes:
    event_type:
      value: ServiceAccountRevoked
`), &root))
	assert.Equal(t, "ServiceAccountRevoked", snsEventType(&root))
}

func TestAsyncEdges_SnsEventType_NilBindings_ReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", snsEventType(nil))
}

func TestAsyncEdges_SnsEventType_NoEventTypeKey_ReturnsEmpty(t *testing.T) {
	var root yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(`
sns:
  messageAttributes: {}
`), &root))
	assert.Equal(t, "", snsEventType(&root))
}

// UnmarshalYAML preserves property order ─────────────────────────────

func TestAsyncEdges_UnmarshalYAML_PreservesPropertyOrder(t *testing.T) {
	var s asyncSchema
	yamlText := `
type: object
properties:
  zebra:
    type: string
  alpha:
    type: integer
  middle:
    type: boolean
`
	require.NoError(t, yaml.Unmarshal([]byte(yamlText), &s))
	assert.Equal(t, []string{"zebra", "alpha", "middle"}, s.PropertyOrder, "declared order preserved")
	assert.Equal(t, 3, len(s.Properties))
}

func TestAsyncEdges_UnmarshalYAML_NoProperties_LeavesOrderEmpty(t *testing.T) {
	var s asyncSchema
	require.NoError(t, yaml.Unmarshal([]byte("type: object\n"), &s))
	assert.Empty(t, s.PropertyOrder)
}

// smoke — cover AsyncAPIHandler by calling it directly (loads embedded spec)
func TestAsyncEdges_AsyncAPIHandler_Renders200(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	AsyncAPIHandler(c)
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.True(t, strings.Contains(w.Body.String(), "<html"))
}
