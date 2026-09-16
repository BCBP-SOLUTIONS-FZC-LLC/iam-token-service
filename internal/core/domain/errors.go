package domain

import "fmt"

// ErrorCode is one of the frozen §17 error taxonomy codes, surfaced through
// platform-gincommon's flat ErrorResponse shape
// ({error, status, trace_id, request_id, details?}).
type ErrorCode string

// ErrorCode values (§17).
const (
	ErrMissingIdentityHeaders ErrorCode = "missing_identity_headers" // 401
	ErrPrincipalNotFound      ErrorCode = "principal_not_found"      // 404
	ErrRotationInFlight       ErrorCode = "rotation_in_flight"       // 409, details.active_rotation_id
	ErrOptimisticLockConflict ErrorCode = "optimistic_lock_conflict" // 409, details.expected_version
	ErrPrincipalRevoked       ErrorCode = "principal_revoked"        // 422
	ErrSecretStoreUnavailable ErrorCode = "secret_store_unavailable" // 502
	ErrInvalidRequest         ErrorCode = "invalid_request"          // 400, details.field

	// ErrDBUnavailable is additive to the frozen §17 taxonomy (same pattern
	// as the sibling Realm Provisioner's ErrDBUnavailable): a low-level
	// Postgres connectivity/resource failure the postgres adapter maps to
	// 503, distinct from a caller's business error.
	ErrDBUnavailable ErrorCode = "db_unavailable" // 503

	// ErrCredentialReplayRevoked is additive to the frozen §17 taxonomy
	// (same pattern as ErrDBUnavailable): TS-1's rotation_id-replay path
	// (§9.2) found the row it would replay, but that credential has since
	// been revoked (a later TS-2, the overlap sweep, or offboarding) and
	// its OpenBao material no longer exists — the replay cannot return the
	// original secret. Distinct from letting the OpenBao read fail and
	// surface as a misleading 502 secret_store_unavailable, which looks
	// like an outage rather than the expected, classifiable outcome of a
	// stale idempotency key.
	// #nosec G101 -- error code name, not a credential value
	ErrCredentialReplayRevoked ErrorCode = "credential_replay_revoked" // 409
)

// httpStatus maps every ErrorCode to its frozen HTTP status (§5.5/§17).
var httpStatus = map[ErrorCode]int{
	ErrMissingIdentityHeaders:  401,
	ErrPrincipalNotFound:       404,
	ErrRotationInFlight:        409,
	ErrOptimisticLockConflict:  409,
	ErrPrincipalRevoked:        422,
	ErrSecretStoreUnavailable:  502,
	ErrInvalidRequest:          400,
	ErrDBUnavailable:           503,
	ErrCredentialReplayRevoked: 409,
}

// Error is this service's domain error type: a stable code, an HTTP status
// derived from that code, a human message, and optional structured details
// echoed verbatim into ErrorResponse.details.
type Error struct {
	Code    ErrorCode
	Message string
	Details map[string]any
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Status returns the frozen HTTP status for e's code.
func (e *Error) Status() int {
	if s, ok := httpStatus[e.Code]; ok {
		return s
	}
	return 500
}

// NewError constructs a domain Error for code with message.
func NewError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message}
}

// WithDetails attaches structured details and returns e for chaining.
func (e *Error) WithDetails(details map[string]any) *Error {
	e.Details = details
	return e
}
