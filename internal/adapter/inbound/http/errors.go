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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

// Domain error type registry (§17). One problem.Type per frozen error code;
// platform-gincommon owns the wire format, this registry owns the IAM-specific
// semantics. Domain is "iam"; names are the §17 codes in kebab-case.
var (
	problemTypeInvalidRequest          = problem.MustType("iam", "invalid-request", "Invalid Request", http.StatusBadRequest, "INVALID_REQUEST")
	problemTypeMissingIdentityHeaders  = problem.MustType("iam", "missing-identity-headers", "Missing Identity Headers", http.StatusUnauthorized, "MISSING_IDENTITY_HEADERS")
	problemTypeTenantPathMismatch      = problem.MustType("iam", "tenant-path-mismatch", "Tenant Path Mismatch", http.StatusForbidden, "TENANT_PATH_MISMATCH")
	problemTypePrincipalNotFound       = problem.MustType("iam", "principal-not-found", "Principal Not Found", http.StatusNotFound, "PRINCIPAL_NOT_FOUND")
	problemTypeRotationInFlight        = problem.MustType("iam", "rotation-in-flight", "Rotation In Flight", http.StatusConflict, "ROTATION_IN_FLIGHT")
	problemTypeOptimisticLockConflict  = problem.MustType("iam", "optimistic-lock-conflict", "Optimistic Lock Conflict", http.StatusConflict, "OPTIMISTIC_LOCK_CONFLICT")
	problemTypeCredentialReplayRevoked = problem.MustType("iam", "credential-replay-revoked", "Credential Replay Revoked", http.StatusConflict, "CREDENTIAL_REPLAY_REVOKED")
	problemTypeCredentialReplayExpired = problem.MustType("iam", "credential-replay-expired", "Credential Replay Expired", http.StatusConflict, "CREDENTIAL_REPLAY_EXPIRED")
	problemTypePrincipalRevoked        = problem.MustType("iam", "principal-revoked", "Principal Revoked", http.StatusUnprocessableEntity, "PRINCIPAL_REVOKED")
	problemTypeSecretStoreUnavailable  = problem.MustType("iam", "secret-store-unavailable", "Secret Store Unavailable", http.StatusBadGateway, "SECRET_STORE_UNAVAILABLE")
	problemTypeDBUnavailable           = problem.MustType("iam", "db-unavailable", "Database Unavailable", http.StatusServiceUnavailable, "DB_UNAVAILABLE")
	problemTypeJWKSKeysUnavailable     = problem.MustType("iam", "jwks-keys-unavailable", "JWKS Keys Unavailable", http.StatusServiceUnavailable, "JWKS_KEYS_UNAVAILABLE")
)

// writeInvalidRequest aborts with a 400 problem carrying one validationErrors
// entry for HTTP-layer parse failures on path and body values. The errorCode
// wire value is the frozen §17 code "invalid_request".
func writeInvalidRequest(c *gin.Context, field, message string) {
	gincommon.AbortWithProblem(c, problem.NewError(problemTypeInvalidRequest, "").
		WithCode(string(domain.ErrInvalidRequest)).
		WithFields(problem.FieldError{Field: field, Message: message}))
}

// writeMissingIdentityHeaders aborts with a 401 problem. The errorCode wire
// value is the frozen §17 code "missing_identity_headers".
func writeMissingIdentityHeaders(c *gin.Context) {
	gincommon.AbortWithProblem(c, problem.NewError(problemTypeMissingIdentityHeaders, "").
		WithCode(string(domain.ErrMissingIdentityHeaders)))
}

// HandleError translates a domain or infrastructure error to an RFC 9457 problem
// and delegates rendering to platform-gincommon. Business semantics (§17) are
// preserved through the problem's errorCode and HTTP status; this function owns
// classification, platform-gincommon owns the wire format.
func HandleError(c *gin.Context, err error) {
	// The request context ended (client disconnect, or a deadline whose 503
	// gincommon's TimeoutMiddleware already wrote). Whatever the database
	// returned is a consequence, not an outage — abort silently.
	if c.Request != nil && c.Request.Context().Err() != nil {
		c.Abort()
		return
	}
	gincommon.AbortWithProblem(c, toProblem(err))
}

// toProblem maps any handler-layer error to a *problem.Error.
func toProblem(err error) *problem.Error {
	var de *domain.Error
	if errors.As(err, &de) {
		return domainErrToProblem(de)
	}
	// Defense in depth: SQLSTATE 55P03 (lock_timeout). The postgres adapter
	// maps 55P03 → rotation_in_flight before errors reach handlers; a leaked
	// one must still classify correctly.
	if isLockNotAvailable(err) {
		return problem.NewError(problemTypeRotationInFlight, "").WithCode(string(domain.ErrRotationInFlight))
	}
	// Defense in depth: SQLSTATE class 08/53/57/58. The adapter maps these to
	// db_unavailable; a leaked one must still produce 503, not 500.
	if isUnavailableSQLState(err) {
		return problem.NewError(problemTypeDBUnavailable, "The database is unavailable.").WithCode(string(domain.ErrDBUnavailable))
	}
	return problem.NewInternalError(err).WithCode("internal_error")
}

// domainErrToProblem converts a typed *domain.Error to a *problem.Error.
// The RFC 9457 type URI identifies the problem class; the errorCode wire
// value is the frozen §17 lowercase code (WithCode preserves the exact
// string callers already depend on, while the Type.Code satisfies the
// platform registry's UPPER_SNAKE_CASE requirement internally).
func domainErrToProblem(de *domain.Error) *problem.Error {
	switch de.Code {
	case domain.ErrInvalidRequest:
		pe := problem.NewError(problemTypeInvalidRequest, de.Message).
			WithCode(string(domain.ErrInvalidRequest))
		if f, ok := de.Details["field"].(string); ok {
			msg, _ := de.Details["reason"].(string)
			if msg == "" {
				msg = de.Message
			}
			pe = pe.WithFields(problem.FieldError{Field: f, Message: msg})
		}
		return pe
	case domain.ErrMissingIdentityHeaders:
		return problem.NewError(problemTypeMissingIdentityHeaders, "").
			WithCode(string(domain.ErrMissingIdentityHeaders))
	case domain.ErrPrincipalNotFound:
		return problem.NewError(problemTypePrincipalNotFound, de.Message).
			WithCode(string(domain.ErrPrincipalNotFound))
	case domain.ErrRotationInFlight:
		detail := de.Message
		if rid, ok := de.Details["active_rotation_id"].(string); ok && rid != "" {
			detail = fmt.Sprintf("A rotation is already in progress (rotation %s).", rid)
		}
		return problem.NewError(problemTypeRotationInFlight, detail).
			WithCode(string(domain.ErrRotationInFlight))
	case domain.ErrOptimisticLockConflict:
		detail := de.Message
		if v, ok := de.Details["expected_version"]; ok {
			detail = fmt.Sprintf("The record was modified since version %v was read.", v)
		}
		return problem.NewError(problemTypeOptimisticLockConflict, detail).
			WithCode(string(domain.ErrOptimisticLockConflict))
	case domain.ErrCredentialReplayRevoked:
		return problem.NewError(problemTypeCredentialReplayRevoked, de.Message).
			WithCode(string(domain.ErrCredentialReplayRevoked))
	case domain.ErrCredentialReplayExpired:
		detail := de.Message
		if v, ok := de.Details["version"]; ok {
			detail = fmt.Sprintf("Credential version %v replay window has expired.", v)
		}
		return problem.NewError(problemTypeCredentialReplayExpired, detail).
			WithCode(string(domain.ErrCredentialReplayExpired))
	case domain.ErrPrincipalRevoked:
		return problem.NewError(problemTypePrincipalRevoked, de.Message).
			WithCode(string(domain.ErrPrincipalRevoked))
	case domain.ErrSecretStoreUnavailable:
		return problem.NewError(problemTypeSecretStoreUnavailable, "The secret store is unavailable.").
			WithCode(string(domain.ErrSecretStoreUnavailable))
	case domain.ErrDBUnavailable:
		return problem.NewError(problemTypeDBUnavailable, "The database is unavailable.").
			WithCode(string(domain.ErrDBUnavailable))
	case domain.ErrJWKSKeysUnavailable:
		return problem.NewError(problemTypeJWKSKeysUnavailable, "The JWKS keys are unavailable.").
			WithCode(string(domain.ErrJWKSKeysUnavailable))
	default:
		return problem.NewInternalError(de).WithCode("internal_error")
	}
}

// ProblemDetails is the RFC 9457 problem details shape emitted for every
// non-2xx response (Content-Type: application/problem+json). Defined here only
// for Swagger documentation — the authoritative implementation is in
// platform-gincommon (pkg/problem.Problem). Handlers never construct this type.
type ProblemDetails struct {
	Type             string           `json:"type" example:"https://errors.company.com/iam/principal-not-found"`
	Title            string           `json:"title" example:"Principal Not Found"`
	Status           int              `json:"status" example:"404"`
	Detail           string           `json:"detail,omitempty" example:"Principal abc123 was not found."`
	Instance         string           `json:"instance,omitempty" example:"/api/v1/internal/tenants/t1/service-accounts/p1"`
	TraceID          string           `json:"traceId,omitempty" example:"4bf92f3577b34da6a3ce929d0e0e4736"`
	ErrorCode        string           `json:"errorCode,omitempty" example:"PRINCIPAL_NOT_FOUND"`
	TenantID         string           `json:"tenantId,omitempty" example:"10000000-0000-0000-0000-000000000001"`
	Timestamp        string           `json:"timestamp,omitempty" example:"2026-10-15T10:45:22Z"`
	ValidationErrors []ValidationItem `json:"validationErrors,omitempty"`
}

// ValidationItem is one entry of the validationErrors extension on a
// 400 INVALID_REQUEST problem (Swagger doc stub — impl in platform-gincommon).
type ValidationItem struct {
	Field   string `json:"field" example:"rotation_id"`
	Message string `json:"message" example:"must be a UUID"`
}

// isUnavailableSQLState reports whether err is a *pgconn.PgError whose SQLSTATE
// class indicates a connectivity or resource failure (08, 53, 57, 58). Used for
// defense-in-depth classification of errors the adapter layer should have wrapped.
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

// isLockNotAvailable reports whether err contains SQLSTATE 55P03 (lock_not_available).
func isLockNotAvailable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}
