package http

import (
	"context"
	"errors"
	"fmt"
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

// A 5xx domain error is logged with its cause and tenant — before, an
// OpenBao outage left only a status code behind. 4xx stay quiet.
func TestHandleError_ServerSideDomainErrorIsLogged(t *testing.T) {
	fl := &fakeLogger{}
	prevLogger := errorLogger
	errorLogger = fl
	defer func() { errorLogger = prevLogger }()

	r := gin.New()
	r.GET("/tenants/:id/probe", func(c *gin.Context) {
		HandleError(c, domain.NewError(domain.ErrSecretStoreUnavailable, "openbao: write_secret: connection refused"))
	})
	r.GET("/tenants/:id/business", func(c *gin.Context) {
		HandleError(c, domain.NewError(domain.ErrPrincipalRevoked, "revoked"))
	})
	tenantID := "8f1c3a2e-0000-4000-8000-000000000001"

	rec := doRequest(t, r, http.MethodGet, "/tenants/"+tenantID+"/probe", nil, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Len(t, fl.errorCalls, 1)
	assert.Contains(t, fl.errorCalls[0]["error"], "write_secret")
	assert.Equal(t, tenantID, fl.errorCalls[0]["tenant_id"])

	doRequest(t, r, http.MethodGet, "/tenants/"+tenantID+"/business", nil, nil)
	assert.Len(t, fl.errorCalls, 1, "a 4xx business outcome is not logged as an error")
}

// A statement cancelled because the request ended is not a database outage.
func TestHandleError_CanceledRequestIsNotDBUnavailable(t *testing.T) {
	fl := &fakeLogger{}
	prevLogger := errorLogger
	errorLogger = fl
	defer func() { errorLogger = prevLogger }()

	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		ctx, cancel := context.WithCancel(c.Request.Context())
		cancel()
		c.Request = c.Request.WithContext(ctx)
		HandleError(c, &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"})
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	assert.NotContains(t, rec.Body.String(), "db_unavailable")
	assert.Empty(t, fl.errorCalls)
}

func TestHandleError_LeakedLockTimeout_Returns409RotationInFlight(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "rotation_in_flight", decodeErrorBody(t, rec).Error)
}

func TestHandleError_CredentialReplayExpired_Is409WithVersion(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, domain.NewError(domain.ErrCredentialReplayExpired, "too late").WithDetails(map[string]any{"version": 3}))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	er := decodeErrorBody(t, rec)
	assert.Equal(t, "credential_replay_expired", er.Error)
	assert.EqualValues(t, 3, er.Details["version"])
}
