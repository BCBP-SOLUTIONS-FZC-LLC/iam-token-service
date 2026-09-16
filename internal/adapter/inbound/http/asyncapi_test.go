package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apispec "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/api"
)

func TestAsyncAPIYAMLHandler(t *testing.T) {
	r := gin.New()
	r.GET("/asyncapi.yaml", AsyncAPIYAMLHandler)
	rec := doRequest(t, r, http.MethodGet, "/asyncapi.yaml", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/yaml")
	assert.Equal(t, string(apispec.AsyncAPISpec), rec.Body.String())
}

// TestAsyncAPIHandler_RendersEmbeddedSpec drives the AsyncAPIHandler end-to-end
// against the real embedded spec. Exercises loadAsyncSpec → readAsyncSpec →
// renderPage → renderServers/renderMessage/renderSchema/renderPropsTable /
// resolveSchema / propType / typeHTML / snsEventType / sortedKeys / walkYAML —
// the entire docs-render surface with a single request.
func TestAsyncAPIHandler_RendersEmbeddedSpec(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, "/asyncapi", nil)

	AsyncAPIHandler(c)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.True(t, len(body) > 500, "rendered doc must be substantial HTML")
	assert.Contains(t, body, "<!DOCTYPE html>")
	assert.Contains(t, body, "AsyncAPI",
		"rendered doc must reference AsyncAPI in the title/body")
	assert.Contains(t, body, "ServiceAccountRegistered",
		"rendered doc must list this service's published events")
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
}

// TestReadAsyncSpec_InvalidYAMLReturnsError covers the parse-error branch
// so readAsyncSpec's error path is not dead code.
func TestReadAsyncSpec_InvalidYAMLReturnsError(t *testing.T) {
	_, err := readAsyncSpec([]byte("::: not-valid ::: yaml"))
	assert.Error(t, err)
}

// TestReadAsyncSpec_MinimalValidYAML covers the happy path with a small
// hand-crafted spec so we don't depend on the embedded bytes for this
// case. Also exercises asyncSchema.UnmarshalYAML property-order capture.
func TestReadAsyncSpec_MinimalValidYAML(t *testing.T) {
	src := []byte(`
asyncapi: "3.0.0"
info:
  title: "Test Spec"
  version: "0.1.0"
components:
  schemas:
    Sample:
      type: object
      required: [id]
      properties:
        id:
          type: string
        name:
          type: string
`)
	spec, err := readAsyncSpec(src)
	require.NoError(t, err)
	assert.Equal(t, "Test Spec", spec.Info.Title)
	assert.Equal(t, "0.1.0", spec.Info.Version)

	sch, ok := spec.Comps.Schemas["Sample"]
	require.True(t, ok)
	// UnmarshalYAML must capture insertion order.
	assert.Equal(t, []string{"id", "name"}, sch.PropertyOrder,
		"schema property order must be preserved for stable render output")
}
