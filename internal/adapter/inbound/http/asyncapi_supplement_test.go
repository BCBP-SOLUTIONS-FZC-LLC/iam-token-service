// asyncapi_supplement_test.go covers the remaining uncovered branches in
// asyncapi.go identified by go test -cover:
//
//   - renderPage: prod/staging env color switch arms (envColor "#ec4b3c" and "#f59e0b")
//   - renderMessage: title fallback when msg.Title == "" (title = name)
//   - renderPropsTable: no-properties branch, sortedKeys fallback when PropertyOrder is empty,
//     guard for property present in map but missing from order slice,
//     itemEnumHTML branch (v.Items != nil && len(v.Items.Enum) > 0)
//
// AsyncAPIHandler's error branch (loadAsyncSpec returns error) is unreachable in
// unit tests because sync.Once is already resolved by the init-time call in
// asyncapi_test.go — not included here.
package http

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── renderPage env color branches ─────────────────────────────────────────────

// minimalSpecForRenderPage returns the simplest valid asyncSpec for renderPage use.
func minimalSpecForRenderPage() *asyncSpec {
	return &asyncSpec{
		AsyncAPI: "3.0.0",
		Info: asyncInfo{
			Title:   "Test",
			Version: "1.0.0",
		},
	}
}

// TestRenderPage_ProdEnv_UsesRedColor verifies that when ENV == "prod" the
// badge background color is the red (#ec4b3c) variant.
func TestRenderPage_ProdEnv_UsesRedColor(t *testing.T) {
	var buf bytes.Buffer
	renderPage(&buf, minimalSpecForRenderPage(), "prod")
	assert.Contains(t, buf.String(), "#ec4b3c",
		"prod env must produce the red (#ec4b3c) badge color")
	assert.Contains(t, buf.String(), "PROD",
		"prod env label must be uppercased to PROD in the badge")
}

// TestRenderPage_ProductionEnv_UsesRedColor verifies the "production" alias.
func TestRenderPage_ProductionEnv_UsesRedColor(t *testing.T) {
	var buf bytes.Buffer
	renderPage(&buf, minimalSpecForRenderPage(), "production")
	assert.Contains(t, buf.String(), "#ec4b3c",
		"production env must produce the red badge color")
}

// TestRenderPage_StagingEnv_UsesAmberColor verifies that when ENV == "staging"
// the badge background color is the amber (#f59e0b) variant.
func TestRenderPage_StagingEnv_UsesAmberColor(t *testing.T) {
	var buf bytes.Buffer
	renderPage(&buf, minimalSpecForRenderPage(), "staging")
	assert.Contains(t, buf.String(), "#f59e0b",
		"staging env must produce the amber (#f59e0b) badge color")
}

// TestRenderPage_StageEnv_UsesAmberColor verifies the "stage" alias.
func TestRenderPage_StageEnv_UsesAmberColor(t *testing.T) {
	var buf bytes.Buffer
	renderPage(&buf, minimalSpecForRenderPage(), "stage")
	assert.Contains(t, buf.String(), "#f59e0b",
		"stage env must produce the amber badge color")
}

// TestRenderPage_EmptyEnv_DefaultsToDevGreen verifies the default (green)
// color and DEV label when ENV is empty.
func TestRenderPage_EmptyEnv_DefaultsToDevGreen(t *testing.T) {
	var buf bytes.Buffer
	renderPage(&buf, minimalSpecForRenderPage(), "")
	out := buf.String()
	assert.Contains(t, out, "#22c55e", "unknown/empty env must produce the green badge color")
	assert.Contains(t, out, "DEV", "empty env label defaults to DEV")
}

// ── renderMessage — title fallback ────────────────────────────────────────────

// TestRenderMessage_EmptyTitle_FallsBackToName verifies that when
// asyncMessage.Title == "", renderMessage uses the message name as the title.
func TestRenderMessage_EmptyTitle_FallsBackToName(t *testing.T) {
	var buf bytes.Buffer
	msg := &asyncMessage{
		Title:   "", // empty → must fall back to name
		Summary: "A brief summary",
	}
	comps := &asyncComponents{}
	renderMessage(&buf, "MyEventName", msg, comps)
	out := buf.String()
	assert.Contains(t, out, "MyEventName",
		"when title is empty the card must show the message name instead")
}

// TestRenderMessage_NonEmptyTitle_UsesTitle verifies that a non-empty title is
// preferred over the fallback.
func TestRenderMessage_NonEmptyTitle_UsesTitle(t *testing.T) {
	var buf bytes.Buffer
	msg := &asyncMessage{
		Title:   "My Custom Title",
		Summary: "",
	}
	comps := &asyncComponents{}
	renderMessage(&buf, "SomeName", msg, comps)
	out := buf.String()
	assert.Contains(t, out, "My Custom Title",
		"non-empty title must be rendered as-is")
}

// ── renderPropsTable — uncovered branches ─────────────────────────────────────

// TestRenderPropsTable_NilSchema_RendersNoProperties verifies the nil-sc guard.
func TestRenderPropsTable_NilSchema_RendersNoProperties(t *testing.T) {
	var buf bytes.Buffer
	renderPropsTable(&buf, nil, "Schema1")
	assert.Contains(t, buf.String(), "No properties",
		"nil schema must render the 'No properties' placeholder")
}

// TestRenderPropsTable_EmptyProperties_RendersNoProperties verifies the
// empty-properties branch (len(sc.Properties) == 0).
func TestRenderPropsTable_EmptyProperties_RendersNoProperties(t *testing.T) {
	var buf bytes.Buffer
	sc := &asyncSchema{Properties: map[string]asyncProp{}}
	renderPropsTable(&buf, sc, "EmptySchema")
	assert.Contains(t, buf.String(), "No properties",
		"schema with zero properties must render the 'No properties' placeholder")
}

// TestRenderPropsTable_NoPropertyOrder_UsesAlphaFallback verifies that when
// PropertyOrder is empty, the fallback to sortedKeys(sc.Properties) renders
// all properties.
func TestRenderPropsTable_NoPropertyOrder_UsesAlphaFallback(t *testing.T) {
	var buf bytes.Buffer
	sc := &asyncSchema{
		// PropertyOrder deliberately left empty so the fallback branch fires.
		Properties: map[string]asyncProp{
			"zeta":  {Type: "string"},
			"alpha": {Type: "integer"},
		},
	}
	renderPropsTable(&buf, sc, "NoOrderSchema")
	out := buf.String()
	assert.Contains(t, out, "zeta")
	assert.Contains(t, out, "alpha")
}

// TestRenderPropsTable_MissingFromOrderSlice_IsAppended verifies the guard
// that appends properties present in the map but absent from PropertyOrder.
func TestRenderPropsTable_MissingFromOrderSlice_IsAppended(t *testing.T) {
	var buf bytes.Buffer
	sc := &asyncSchema{
		PropertyOrder: []string{"alpha"}, // "beta" intentionally omitted
		Properties: map[string]asyncProp{
			"alpha": {Type: "string"},
			"beta":  {Type: "boolean"}, // must be appended by the guard
		},
	}
	renderPropsTable(&buf, sc, "GuardSchema")
	out := buf.String()
	assert.Contains(t, out, "alpha", "explicitly ordered property must be rendered")
	assert.Contains(t, out, "beta", "property missing from order slice must be appended and rendered")
}

// TestRenderPropsTable_ItemEnumValues_RendersArrayItemEnums verifies the
// itemEnumHTML branch: when a property is an array whose Items carry an Enum,
// the enum values are rendered inline in the description column.
func TestRenderPropsTable_ItemEnumValues_RendersArrayItemEnums(t *testing.T) {
	var buf bytes.Buffer
	sc := &asyncSchema{
		Properties: map[string]asyncProp{
			"roles": {
				Type: "array",
				Items: &asyncProp{
					Type: "string",
					Enum: []string{"tenant_owner", "tenant_admin", "member"},
				},
			},
		},
	}
	renderPropsTable(&buf, sc, "ItemEnumSchema")
	out := buf.String()
	assert.True(t, strings.Contains(out, "tenant_owner"),
		"array item enum values must be rendered in the property row")
	assert.True(t, strings.Contains(out, "tenant_admin"))
	assert.True(t, strings.Contains(out, "member"))
}

// TestRenderPropsTable_RequiredField_MarkedInTable verifies that a required
// property is marked as "required" in the rendered HTML.
func TestRenderPropsTable_RequiredField_MarkedInTable(t *testing.T) {
	var buf bytes.Buffer
	sc := &asyncSchema{
		Required: []string{"id"},
		Properties: map[string]asyncProp{
			"id":   {Type: "string"},
			"name": {Type: "string"},
		},
	}
	renderPropsTable(&buf, sc, "RequiredSchema")
	out := buf.String()
	assert.Contains(t, out, "required",
		"required field must be marked in the property table")
}

// TestRenderPropsTable_ExampleField_Rendered verifies that when a property
// carries an Example value it is rendered as a <pre> code block.
func TestRenderPropsTable_ExampleField_Rendered(t *testing.T) {
	var buf bytes.Buffer
	sc := &asyncSchema{
		Properties: map[string]asyncProp{
			"count": {Type: "integer", Example: float64(42)},
		},
	}
	renderPropsTable(&buf, sc, "ExampleSchema")
	out := buf.String()
	assert.Contains(t, out, "42", "example value must appear in rendered output")
}

// TestRenderPropsTable_EnumField_Rendered verifies that when a property
// carries its own Enum values they are rendered as badge spans.
func TestRenderPropsTable_EnumField_Rendered(t *testing.T) {
	var buf bytes.Buffer
	sc := &asyncSchema{
		Properties: map[string]asyncProp{
			"status": {
				Type: "string",
				Enum: []string{"active", "suspended", "left"},
			},
		},
	}
	renderPropsTable(&buf, sc, "EnumSchema")
	out := buf.String()
	assert.True(t, strings.Contains(out, "active") && strings.Contains(out, "suspended"),
		"enum values must be rendered as badge spans")
}

// TestReadAsyncSpec_SchemaWithPropertiesNode_CapturesOrder verifies that
// UnmarshalYAML's property-order capture branch triggers when a schema has
// a properties node that is NOT captured by any existing test — specifically
// the branch where propsNode.Kind == yaml.MappingNode is false (i.e. the node
// has no properties key at all), confirming PropertyOrder stays nil without panic.
func TestReadAsyncSpec_SchemaWithNoPropertiesKey_LeavesOrderNil(t *testing.T) {
	src := []byte(`
asyncapi: "3.0.0"
info:
  title: "T"
  version: "1"
components:
  schemas:
    Plain:
      type: string
`)
	spec, err := readAsyncSpec(src)
	require.NoError(t, err)
	sc := spec.Comps.Schemas["Plain"]
	assert.Empty(t, sc.PropertyOrder,
		"a schema node with no 'properties' key must leave PropertyOrder empty")
}
