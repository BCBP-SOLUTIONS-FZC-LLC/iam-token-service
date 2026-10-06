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

	// ErrCredentialReplayExpired is additive to the frozen §17 taxonomy
	// (TS-D22, same pattern as ErrCredentialReplayRevoked): a TS-1
	// rotation_id replay (§9.2) arrived after the replay window
	// (ROTATION_REPLAY_WINDOW) measured from the credential's issued_at.
	// The credential may still be live, but its private key is not returned
	// again — a rotation_id is a retry key, not a standing read handle.
	// Distinct from credential_replay_revoked so a caller can tell "too
	// late" from "gone"; details.version names the credential.
	// #nosec G101 -- error code name, not a credential value
	ErrCredentialReplayExpired ErrorCode = "credential_replay_expired" // 409

	// ErrJWKSKeysUnavailable is additive to the frozen §17 taxonomy (TS-D23,
	// same pattern as ErrDBUnavailable): the EXT-6 JWKS route could not read
	// the tenant's ACTIVE key, or could not read any live key. A 200 then
	// would hand Keycloak a set without the key the automation principal is
	// signing with today, and Keycloak would cache it and reject every
	// client_assertion; a 503 makes it keep its previous keys. Replaces the
	// route's earlier use of secret_store_unavailable, whose frozen status is
	// 502, not 503 — that code keeps its 502 on every other route.
	ErrJWKSKeysUnavailable ErrorCode = "jwks_keys_unavailable" // 503
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
	ErrCredentialReplayExpired: 409,
	ErrJWKSKeysUnavailable:     503,
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
