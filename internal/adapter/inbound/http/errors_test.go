package http

import (
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func TestHandleError_DomainError(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, domain.NewError(domain.ErrPrincipalRevoked, "revoked").WithDetails(map[string]any{"x": "y"}))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	er := decodeErrorBody(t, rec)
	assert.Equal(t, "principal_revoked", er.Error)
	assert.Equal(t, http.StatusUnprocessableEntity, er.Status)
	assert.Equal(t, "y", er.Details["x"])
}

func TestHandleError_UnclassifiedError_LogsAndReturns500(t *testing.T) {
	fl := &fakeLogger{}
	prevLogger := errorLogger
	errorLogger = fl
	defer func() { errorLogger = prevLogger }()

	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, errors.New("boom"))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	er := decodeErrorBody(t, rec)
	assert.Equal(t, "internal_error", er.Error)
	assert.Nil(t, er.Details)
	require.Len(t, fl.errorCalls, 1)
	assert.Equal(t, "boom", fl.errorCalls[0]["error"])
}

// TestHandleError_UnclassifiedError_WithTraceIDLogsTraceID covers the
// gincommon.TraceIDFromContext(c) != "" branch — the prior unclassified-error
// test ran with no tracing middleware installed, so trace_id was always
// empty.
func TestHandleError_UnclassifiedError_WithTraceIDLogsTraceID(t *testing.T) {
	fl := &fakeLogger{}
	prevLogger := errorLogger
	errorLogger = fl
	defer func() { errorLogger = prevLogger }()

	r := gin.New()
	r.Use(gincommon.ObservabilityMiddlewares(testGinConfig)...)
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, errors.New("boom"))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Len(t, fl.errorCalls, 1)
	assert.NotEmpty(t, fl.errorCalls[0]["trace_id"])
}

func TestHandleError_UnclassifiedError_NilLoggerDoesNotPanic(t *testing.T) {
	prevLogger := errorLogger
	errorLogger = nil
	defer func() { errorLogger = prevLogger }()

	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, errors.New("boom"))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestHandleError_LeakedConnectivitySQLState_Returns503(t *testing.T) {
	for _, code := range []string{"08006", "08001", "53300", "57P01", "58030"} {
		t.Run(code, func(t *testing.T) {
			r := gin.New()
			r.GET("/probe", func(c *gin.Context) {
				HandleError(c, &pgconn.PgError{Code: code, Message: "boom"})
			})
			rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			er := decodeErrorBody(t, rec)
			assert.Equal(t, "db_unavailable", er.Error)
		})
	}
}

// TestHandleError_LeakedConnectivitySQLState_LogsWhenLoggerSet covers the
// errorLogger != nil branch on the db_unavailable path — the 503 test above
// runs with no logger installed, so that branch was never exercised.
func TestHandleError_LeakedConnectivitySQLState_LogsWhenLoggerSet(t *testing.T) {
	fl := &fakeLogger{}
	prevLogger := errorLogger
	errorLogger = fl
	defer func() { errorLogger = prevLogger }()

	r := gin.New()
	r.Use(gincommon.ObservabilityMiddlewares(testGinConfig)...)
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, &pgconn.PgError{Code: "08006", Message: "boom"})
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Len(t, fl.errorCalls, 1)
	assert.NotEmpty(t, fl.errorCalls[0]["trace_id"])
}

func TestHandleError_LeakedConstraintSQLState_Returns500(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, &pgconn.PgError{Code: "23505", Message: "duplicate"})
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	er := decodeErrorBody(t, rec)
	assert.Equal(t, "internal_error", er.Error)
}

func TestWriteInvalidRequest(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		writeInvalidRequest(c, "some_field", "some reason")
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	er := decodeErrorBody(t, rec)
	assert.Equal(t, "invalid_request", er.Error)
	assert.Equal(t, "some_field", er.Details["field"])
	assert.Equal(t, "some reason", er.Details["reason"])
}

func TestNewErrorResponse_TraceAndRequestIDPropagation(t *testing.T) {
	t.Run("gincommon.RequestIDFromContext branch (RequestIDMiddleware ran)", func(t *testing.T) {
		r := gin.New()
		r.Use(gincommon.ObservabilityMiddlewares(testGinConfig)...)
		r.GET("/probe", func(c *gin.Context) {
			writeError(c, "some_code", http.StatusTeapot, nil)
		})
		rec := doRequest(t, r, http.MethodGet, "/probe", map[string]string{"x-request-id": "ctx-req-id"}, nil)
		require.Equal(t, http.StatusTeapot, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "ctx-req-id", er.RequestID)
	})

	t.Run("request header fallback branch (no RequestIDMiddleware in chain)", func(t *testing.T) {
		r := gin.New()
		r.GET("/probe", func(c *gin.Context) {
			writeError(c, "some_code", http.StatusTeapot, nil)
		})
		rec := doRequest(t, r, http.MethodGet, "/probe", map[string]string{gincommon.HeaderRequestID: "hdr-req-id"}, nil)
		require.Equal(t, http.StatusTeapot, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "hdr-req-id", er.RequestID)
	})

	t.Run("response header fallback branch (neither context nor request header set)", func(t *testing.T) {
		r := gin.New()
		r.GET("/probe", func(c *gin.Context) {
			c.Writer.Header().Set(gincommon.HeaderRequestIDResponse, "resp-only-id")
			writeError(c, "some_code", http.StatusTeapot, nil)
		})
		rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
		require.Equal(t, http.StatusTeapot, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "resp-only-id", er.RequestID)
	})

	t.Run("no request id available anywhere — empty, no panic", func(t *testing.T) {
		r := gin.New()
		r.GET("/probe", func(c *gin.Context) {
			writeError(c, "some_code", http.StatusTeapot, nil)
		})
		rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
		require.Equal(t, http.StatusTeapot, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Empty(t, er.RequestID)
	})
}

func TestWriteMissingIdentityHeaders(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) { writeMissingIdentityHeaders(c) })
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	er := decodeErrorBody(t, rec)
	assert.Equal(t, "missing_identity_headers", er.Error)
	assert.Nil(t, er.Details)
}
