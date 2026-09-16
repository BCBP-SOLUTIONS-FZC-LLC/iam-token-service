package http

import (
	"net/http"
	"strings"
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

func TestAsyncAPIHandler(t *testing.T) {
	r := gin.New()
	r.GET("/asyncapi", AsyncAPIHandler)
	rec := doRequest(t, r, http.MethodGet, "/asyncapi", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html")

	body := rec.Body.String()
	assert.Contains(t, body, "iam-token-service")
	// The embedded spec's own YAML syntax uses "<" nowhere by construction,
	// but html.EscapeString must still have run — assert a recognizable,
	// escaped fragment of the spec survives inside the <pre> block instead
	// of asserting a negative (absence of "<") that would pass vacuously.
	assert.Contains(t, body, "asyncapi: 3.0.0")
	assert.True(t, strings.Contains(body, "<pre>") && strings.Contains(body, "</pre>"))
}
