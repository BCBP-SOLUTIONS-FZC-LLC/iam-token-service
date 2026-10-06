// Package http implements the inbound HTTP surface: Gin handlers and the
// middleware chain that binds gateway-injected identity into the request
// context and the DB pool's RLS GUC. router.go is the single source of
// truth for this service's routes — cmd/server's job is to construct
// dependencies and call NewRouter, not to encode routing decisions itself.
package http

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/jackc/pgx/v5/pgconn"
)

// errorLogger is the shared gincommon-backed Logger, set once by NewRouter.
var errorLogger port.Logger

// ErrorResponse is the flat wire shape (§17): {error, status, trace_id,
// request_id, details?}. platform-gincommon's own ErrorResponse has no
// details field, so this service defines its own to carry
// details.field / details.expected_version / details.active_rotation_id.
type ErrorResponse struct {
	Error     string         `json:"error"`
	Status    int            `json:"status"`
	TraceID   string         `json:"trace_id,omitempty"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

func newErrorResponse(c *gin.Context, code string, status int, details map[string]any) ErrorResponse {
	er := ErrorResponse{Error: code, Status: status, Details: details}
	if c != nil {
		er.TraceID = gincommon.TraceIDFromContext(c)
		if rid := gincommon.RequestIDFromContext(c); rid != "" {
			er.RequestID = rid
		} else if rid := c.GetHeader(gincommon.HeaderRequestID); rid != "" {
			er.RequestID = rid
		} else if rid := c.Writer.Header().Get(gincommon.HeaderRequestIDResponse); rid != "" {
			er.RequestID = rid
		}
	}
	return er
}

// writeError writes code/status/details as the §17 wire body and aborts
// the request.
func writeError(c *gin.Context, code string, status int, details map[string]any) {
	c.AbortWithStatusJSON(status, newErrorResponse(c, code, status, details))
}

// writeInvalidRequest writes a 400 invalid_request with details.field —
// the shared shape for both HTTP-layer type-parse failures (path/body
// values that don't even parse into a UUID/int) and the service layer's
// own §5.1 content-validation errors (e.g. keycloak_client_id mismatch).
func writeInvalidRequest(c *gin.Context, field, reason string) {
	writeError(c, string(domain.ErrInvalidRequest), http.StatusBadRequest, map[string]any{"field": field, "reason": reason})
}

// HandleError maps a *domain.Error to its frozen §17 status/code, or falls
// back to 500 internal_error for anything unclassified (logged so an
// unhandled error path is never silent). wrapConnErr maps SQLSTATE
// 08/53/57/58 to domain.ErrDBUnavailable before an error can reach a
// handler (§9.4), so the *domain.Error branch is the production path.
// The pgcommon fallback still classifies a leaked PgError (tests, future
// missed wrap) into 503 db_unavailable without importing pgconn —
// matching iam-user-profile / iam-org-membership.
func HandleError(c *gin.Context, err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		// A 5xx domain error (secret_store_unavailable, db_unavailable, …)
		// carries its cause only in the error text — log it, or an outage
		// leaves nothing but a status code behind. 4xx are expected
		// business outcomes and stay quiet. Messages never contain secret
		// material (TS-INV-2): adapters wrap op names and SDK errors only.
		if de.Status() >= http.StatusInternalServerError {
			logRequestError(c, "request failed: "+string(de.Code), err)
		}
		writeError(c, string(de.Code), de.Status(), de.Details)
		return
	}
	// The request context ended (client disconnect, or the request deadline
	// whose 503 gincommon's TimeoutMiddleware already wrote): whatever the
	// database returned is a consequence, not a database outage.
	if c.Request != nil && c.Request.Context().Err() != nil {
		c.Abort()
		return
	}
	// Defense in depth, like the 503 branch below: the postgres adapter maps
	// SQLSTATE 55P03 (lock_timeout on a principal row another credential
	// write holds) to rotation_in_flight, and a leaked one must still be the
	// retryable 409, not a 500.
	if isLockNotAvailable(err) {
		writeError(c, string(domain.ErrRotationInFlight), http.StatusConflict, nil)
		return
	}
	if isUnavailableSQLState(err) {
		logRequestError(c, "database unavailable", err)
		writeError(c, string(domain.ErrDBUnavailable), http.StatusServiceUnavailable, nil)
		return
	}
	logRequestError(c, "unhandled 500 error", err)
	writeError(c, "internal_error", http.StatusInternalServerError, nil)
}

func logRequestError(c *gin.Context, msg string, err error) {
	if errorLogger == nil {
		return
	}
	fields := map[string]interface{}{"error_type": fmt.Sprintf("%T", err), "error": err.Error()}
	if tid := gincommon.TraceIDFromContext(c); tid != "" {
		fields["trace_id"] = tid
	}
	if tenantID := c.Param("id"); tenantID != "" {
		fields["tenant_id"] = tenantID
	}
	if route := c.FullPath(); route != "" {
		fields["route"] = route
	}
	errorLogger.Error(msg, fields)
}

// isUnavailableSQLState is defense in depth for a *pgconn.PgError that
// escaped the repository layer's wrapConnErr: SQLSTATE class 08/53/57/58,
// matched on the code, never on error text.
func isUnavailableSQLState(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "08", "53", "57", "58":
		return true
	}
	return false
}

// isLockNotAvailable reports SQLSTATE 55P03 (lock_not_available).
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}
