package http

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/pkg/requestctx"
)

// CredentialService is the subset of *service.CredentialService this
// handler calls — an interface (rather than the concrete type) so
// cmd/server can inject a metrics-instrumented decorator
// (iam_token_service_credentials_issued_total, §11.2) without this
// package depending on the metrics package: adapters_inbound may not
// depend on adapters_outbound (.go-arch-lint.yml). *service.CredentialService
// satisfies this interface as-is — no call site outside this package
// changes.
type CredentialService interface {
	IssueOrRotate(ctx context.Context, tenantID, principalID uuid.UUID, req service.IssueOrRotateRequest, actor uuid.UUID) (*service.IssueOrRotateResult, error)
	Revoke(ctx context.Context, tenantID, principalID uuid.UUID, version int, actor uuid.UUID) (*service.RevokeResult, error)
}

// CredentialHandler implements TS-1 (issue/rotate) and TS-2 (revoke) (§5.4).
type CredentialHandler struct {
	svc            CredentialService
	defaultOverlap int
}

// NewCredentialHandler constructs a CredentialHandler. An omitted
// overlap_seconds uses domain.DefaultOverlapSeconds unless
// WithDefaultOverlapSeconds overrides it.
func NewCredentialHandler(svc CredentialService) *CredentialHandler {
	return &CredentialHandler{svc: svc, defaultOverlap: domain.DefaultOverlapSeconds}
}

// WithDefaultOverlapSeconds sets the overlap applied when a TS-1 body omits
// overlap_seconds (ROTATION_DEFAULT_OVERLAP_SECONDS, §12). The service
// still clamps it to [0, 900] (TS-CONFIG-4).
func (h *CredentialHandler) WithDefaultOverlapSeconds(seconds int) *CredentialHandler {
	h.defaultOverlap = seconds
	return h
}

type issueOrRotateRequestBody struct {
	RotationID     string `json:"rotation_id"`
	OverlapSeconds *int   `json:"overlap_seconds"`
}

type issueOrRotateResponseBody struct {
	Version        int     `json:"version"`
	Secret         string  `json:"secret"`
	OpenBaoPath    string  `json:"openbao_path"`
	ExpiresPriorAt *string `json:"expires_prior_at,omitempty"`
	RecordVersion  int     `json:"record_version"`
}

// IssueOrRotate implements TS-1: POST
// …/service-accounts/:principal_id/credentials (§5.4) — returns the
// generated plaintext exactly once (TS-INV-2). `secret` (field name kept
// for wire stability) is a PEM-encoded RSA private key (EXT-6, §2.5), not a
// shared-secret string: the platform-automation principal authenticates to
// Keycloak via client-jwt against a JWKS this service serves at
// .../service-accounts/platform-automation/jwks.json, derived from this and
// every other live (active/rotating) credential's public half — the
// private key itself never leaves this response.
//
// @Summary      TS-1 — Issue or rotate credential
// @Description  Issues the principal's first credential (version=1, 201) or rotates to a new version (201) when one already exists. A rotation_id replay of an already-committed request returns the stored result (200) instead of generating new material (§9.2) — only within ROTATION_REPLAY_WINDOW (default 15m) of the credential's issue; later it is 409 credential_replay_expired, and 409 credential_replay_revoked once that version is revoked. Success responses carry Cache-Control: no-store and Pragma: no-cache. Returns a PEM-encoded RSA private key exactly once — it is never retrievable again (TS-INV-2); the matching public key is served at the principal's JWKS endpoint (EXT-6, §2.5) once RP-17 refreshes Keycloak's keys cache. Requires x-user-id = the fixed iam-system principal UUID 00000000-0000-0000-0000-0000000000a1 (domain.SystemPrincipalID; any other value is 401 missing_identity_headers) and x-tenant-id equal to the {id} path segment (else 403 tenant_path_mismatch).
// @Tags         Credentials
// @Accept       json
// @Produce      json
// @Param        id            path      string                     true  "Tenant UUID"     format(uuid)
// @Param        principal_id  path      string                     true  "Principal UUID"  format(uuid)
// @Param        request       body      issueOrRotateRequestBody  true  "rotation_id required; overlap_seconds optional (defaults to the configured value, §12)"
// @Success      200           {object}  issueOrRotateResponseBody  "rotation_id replay — already committed"
// @Success      201           {object}  issueOrRotateResponseBody  "issued or rotated"
// @Failure      400  {object}  ErrorResponse  "invalid_request"
// @Failure      401  {object}  ErrorResponse  "missing_identity_headers"
// @Failure      403  {object}  ErrorResponse  "tenant_path_mismatch"
// @Failure      404  {object}  ErrorResponse  "principal_not_found"
// @Failure      409  {object}  ErrorResponse  "rotation_in_flight (retryable; incl. principal lock wait timeout) | optimistic_lock_conflict | credential_replay_revoked | credential_replay_expired — details.version on the replay codes"
// @Failure      415  {object}  ErrorResponse  "unsupported_media_type"
// @Failure      422  {object}  ErrorResponse  "principal_revoked"
// @Failure      500  {object}  ErrorResponse
// @Failure      502  {object}  ErrorResponse  "secret_store_unavailable"
// @Failure      503  {object}  ErrorResponse  "db_unavailable"
// @Security     TenantID && UserID
// @Router       /tenants/{id}/service-accounts/{principal_id}/credentials [post]
func (h *CredentialHandler) IssueOrRotate(c *gin.Context) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		writeMissingIdentityHeaders(c)
		return
	}

	principalID, err := uuid.Parse(c.Param("principal_id"))
	if err != nil {
		writeInvalidRequest(c, "principal_id", "must be a UUID")
		return
	}

	var body issueOrRotateRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidRequest(c, "body", "malformed JSON body")
		return
	}

	// An omitted/empty rotation_id is passed through as uuid.Nil so the
	// service layer's own "rotation_id is required" check (§5.1) fires
	// with the same 400 invalid_request shape — only a present-but-
	// unparseable value is rejected here, at the JSON-string boundary.
	var rotationID uuid.UUID
	if body.RotationID != "" {
		rotationID, err = uuid.Parse(body.RotationID)
		if err != nil {
			writeInvalidRequest(c, "rotation_id", "must be a UUID")
			return
		}
	}

	// overlap_seconds: nil (omitted) means "use the configured default"
	// (ROTATION_DEFAULT_OVERLAP_SECONDS, §12, 300s) — distinct from an
	// explicit 0 (a hard cutover, §6.2).
	// domain.ClampOverlapSeconds still bounds whatever value reaches the
	// service layer to [0, 900] regardless of source (TS-CONFIG-4).
	overlap := h.defaultOverlap
	if body.OverlapSeconds != nil {
		overlap = *body.OverlapSeconds
	}

	res, err := h.svc.IssueOrRotate(c.Request.Context(), rc.TenantID, principalID, service.IssueOrRotateRequest{
		RotationID: rotationID, OverlapSeconds: overlap,
	}, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}

	var expiresPriorAt *string
	if res.ExpiresPriorAt != nil {
		s := res.ExpiresPriorAt.UTC().Format(time.RFC3339)
		expiresPriorAt = &s
	}

	// §5.5 documents 201 for "first-time creation"; a rotation_id replay
	// (§9.2) echoes the already-committed result, so it reports 200 rather
	// than implying a second creation occurred.
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	// The body carries a private key: no browser, proxy or client cache may
	// keep a copy of it (TS-INV-2 — returned exactly once).
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(status, issueOrRotateResponseBody{
		Version: res.Version, Secret: res.Secret, OpenBaoPath: res.OpenBaoPath,
		ExpiresPriorAt: expiresPriorAt, RecordVersion: res.RecordVersion,
	})
}

type revokeResponseBody struct {
	Version              int    `json:"version"`
	Status               string `json:"status"`
	RevokedAt            string `json:"revoked_at"`
	KeycloakInvalidation string `json:"keycloak_invalidation"`
}

// Revoke implements TS-2: POST
// …/service-accounts/:principal_id/credentials/:version/revoke (§5.4).
// keycloak_invalidation is always "caller_responsibility" — this service
// never invalidates the secret at Keycloak (two-halves, TS-INV-7, §6.1).
//
// @Summary      TS-2 — Revoke credential version
// @Description  Revokes one credential version — deletes its OpenBao material and marks it revoked. keycloak_invalidation is always "caller_responsibility": this service never invalidates the token at Keycloak itself (two-halves invariant, TS-INV-7). Requires x-user-id = the fixed iam-system principal UUID 00000000-0000-0000-0000-0000000000a1 (domain.SystemPrincipalID; any other value is 401 missing_identity_headers) and x-tenant-id equal to the {id} path segment (else 403 tenant_path_mismatch).
// @Tags         Credentials
// @Produce      json
// @Param        id            path      string  true  "Tenant UUID"      format(uuid)
// @Param        principal_id  path      string  true  "Principal UUID"   format(uuid)
// @Param        version       path      int     true  "Credential version"
// @Success      200           {object}  revokeResponseBody
// @Failure      400  {object}  ErrorResponse  "invalid_request"
// @Failure      401  {object}  ErrorResponse  "missing_identity_headers"
// @Failure      403  {object}  ErrorResponse  "tenant_path_mismatch"
// @Failure      404  {object}  ErrorResponse  "principal_not_found"
// @Failure      409  {object}  ErrorResponse  "rotation_in_flight (retryable; principal lock wait timeout) | optimistic_lock_conflict"
// @Failure      415  {object}  ErrorResponse  "unsupported_media_type (only when a body is sent)"
// @Failure      500  {object}  ErrorResponse
// @Failure      502  {object}  ErrorResponse  "secret_store_unavailable"
// @Failure      503  {object}  ErrorResponse  "db_unavailable"
// @Security     TenantID && UserID
// @Router       /tenants/{id}/service-accounts/{principal_id}/credentials/{version}/revoke [post]
func (h *CredentialHandler) Revoke(c *gin.Context) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		writeMissingIdentityHeaders(c)
		return
	}

	principalID, err := uuid.Parse(c.Param("principal_id"))
	if err != nil {
		writeInvalidRequest(c, "principal_id", "must be a UUID")
		return
	}
	version, err := strconv.Atoi(c.Param("version"))
	if err != nil || version < 1 {
		writeInvalidRequest(c, "version", "must be a positive integer")
		return
	}

	res, err := h.svc.Revoke(c.Request.Context(), rc.TenantID, principalID, version, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}

	c.JSON(http.StatusOK, revokeResponseBody{
		Version: res.Version, Status: string(res.Status),
		RevokedAt:            res.RevokedAt.UTC().Format(time.RFC3339),
		KeycloakInvalidation: "caller_responsibility",
	})
}
