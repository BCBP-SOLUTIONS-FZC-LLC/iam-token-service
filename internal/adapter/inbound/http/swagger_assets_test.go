package http

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSwaggerInitializerHandler(t *testing.T) {
	r := gin.New()
	r.GET("/swagger-initializer.js", SwaggerInitializerHandler)
	rec := doRequest(t, r, http.MethodGet, "/swagger-initializer.js", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/javascript")
	assert.NotEmpty(t, rec.Body.String())
}

func TestSwaggerThemeHandler(t *testing.T) {
	r := gin.New()
	r.GET("/index.css", SwaggerThemeHandler)
	rec := doRequest(t, r, http.MethodGet, "/index.css", nil, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/css")
	assert.NotEmpty(t, rec.Body.String())
}
