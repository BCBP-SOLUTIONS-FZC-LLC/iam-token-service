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
// unhandled error path is never silent). The postgres adapter's wrapConnErr
// already maps every connectivity/resource SQLSTATE to
// domain.ErrDBUnavailable before an error can reach a handler (§9.4), so
// the *domain.Error branch below is the only expected production path.
func HandleError(c *gin.Context, err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		writeError(c, string(de.Code), de.Status(), de.Details)
		return
	}
	if errorLogger != nil {
		fields := map[string]interface{}{"error_type": fmt.Sprintf("%T", err), "error": err.Error()}
		if tid := gincommon.TraceIDFromContext(c); tid != "" {
			fields["trace_id"] = tid
		}
		errorLogger.Error("unhandled 500 error", fields)
	}
	writeError(c, "internal_error", http.StatusInternalServerError, nil)
}
