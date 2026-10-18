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
)

func TestHandleError_DomainError(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, domain.NewError(domain.ErrPrincipalRevoked, "revoked"))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	p := decodeErrorBody(t, rec)
	assert.Equal(t, "principal_revoked", p.ErrorCode)
	assert.Equal(t, http.StatusUnprocessableEntity, p.Status)
	assert.Equal(t, "revoked", p.Detail)
}

func TestHandleError_UnclassifiedError_Returns500(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, errors.New("boom"))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	p := decodeErrorBody(t, rec)
	assert.Equal(t, "internal_error", p.ErrorCode)
	assert.Empty(t, p.Detail, "internal-error detail is never sent to the client")
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
			p := decodeErrorBody(t, rec)
			assert.Equal(t, "db_unavailable", p.ErrorCode)
		})
	}
}

func TestHandleError_LeakedConstraintSQLState_Returns500(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, &pgconn.PgError{Code: "23505", Message: "duplicate"})
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	p := decodeErrorBody(t, rec)
	assert.Equal(t, "internal_error", p.ErrorCode)
}

func TestWriteInvalidRequest(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		writeInvalidRequest(c, "some_field", "some reason")
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	p := decodeErrorBody(t, rec)
	assert.Equal(t, "invalid_request", p.ErrorCode)
	require.Len(t, p.ValidationErrors, 1)
	assert.Equal(t, "some_field", p.ValidationErrors[0].Field)
	assert.Equal(t, "some reason", p.ValidationErrors[0].Message)
}

func TestWriteMissingIdentityHeaders(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) { writeMissingIdentityHeaders(c) })
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	p := decodeErrorBody(t, rec)
	assert.Equal(t, "missing_identity_headers", p.ErrorCode)
	assert.Empty(t, p.ValidationErrors)
}

// A statement cancelled because the request ended is not a database outage —
// HandleError aborts without writing a body so gincommon's TimeoutMiddleware's
// 503 (or the empty response on client disconnect) is not replaced.
func TestHandleError_CanceledRequestIsNotDBUnavailable(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		ctx, cancel := context.WithCancel(c.Request.Context())
		cancel()
		c.Request = c.Request.WithContext(ctx)
		HandleError(c, &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"})
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	assert.NotContains(t, rec.Body.String(), "db_unavailable")
	assert.NotContains(t, rec.Body.String(), "DB_UNAVAILABLE")
}

func TestHandleError_LeakedLockTimeout_Returns409RotationInFlight(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "rotation_in_flight", decodeErrorBody(t, rec).ErrorCode)
}

func TestHandleError_CredentialReplayExpired_Is409WithVersion(t *testing.T) {
	r := gin.New()
	r.GET("/probe", func(c *gin.Context) {
		HandleError(c, domain.NewError(domain.ErrCredentialReplayExpired, "too late").WithDetails(map[string]any{"version": 3}))
	})
	rec := doRequest(t, r, http.MethodGet, "/probe", nil, nil)
	require.Equal(t, http.StatusConflict, rec.Code)
	p := decodeErrorBody(t, rec)
	assert.Equal(t, "credential_replay_expired", p.ErrorCode)
	assert.Contains(t, p.Detail, "version 3")
}
