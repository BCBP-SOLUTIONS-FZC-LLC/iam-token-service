package http

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/pkg/requestctx"
)

// PrincipalService is the subset of *service.PrincipalService this handler
// calls — an interface (rather than the concrete type), mirroring
// CredentialHandler's identical pattern: it keeps this handler unit-testable
// against a hand-written fake without standing up real port
// implementations. *service.PrincipalService satisfies this interface
// as-is — no call site outside this package changes.
type PrincipalService interface {
	Register(ctx context.Context, tenantID uuid.UUID, req service.RegisterRequest, actor uuid.UUID) (*service.RegisterResult, error)
	ReadPrincipal(ctx context.Context, tenantID, principalID uuid.UUID) (*service.ReadPrincipalResult, error)
	FindPrincipalBySub(ctx context.Context, tenantID, principalSub uuid.UUID) (*service.FindBySubResult, error)
	ReadPlatformAutomation(ctx context.Context, tenantID uuid.UUID) (*service.AutomationPrincipalResult, error)
}

// PrincipalHandler implements TS-3 (read), TS-4 (register), TS-5
// (find-by-sub, AUTH-9), and TS-6 (read the automation principal,
// TS-D17) (§5.4).
type PrincipalHandler struct {
	svc PrincipalService
}

// NewPrincipalHandler constructs a PrincipalHandler.
func NewPrincipalHandler(svc PrincipalService) *PrincipalHandler {
	return &PrincipalHandler{svc: svc}
}

type registerRequestBody struct {
	PrincipalSub     string `json:"principal_sub"`
	KeycloakClientID string `json:"keycloak_client_id"`
}

type principalResponseBody struct {
	PrincipalID   uuid.UUID `json:"principal_id"`
	TenantID      uuid.UUID `json:"tenant_id"`
	PrincipalType string    `json:"principal_type"`
	Status        string    `json:"status"`
	RecordVersion int       `json:"record_version"`
}

// Register implements TS-4: POST /api/v1/internal/tenants/:id/service-accounts
// (§5.4). 201 on first creation, 200 on an idempotent repeat.
//
// @Summary      TS-4 — Register service-account principal
// @Description  Registers the per-tenant platform-automation service-account principal. Idempotent on (tenant_id, principal_type) — repeating an already-registered request returns 200 with the existing record, and a re-mint (new principal_sub/keycloak_client_id) updates it in place, keeping principal_id. Requires x-user-id = the fixed iam-system principal UUID 00000000-0000-0000-0000-0000000000a1 (domain.SystemPrincipalID; any other value is 401 missing_identity_headers) and x-tenant-id equal to the {id} path segment (else 403 tenant_path_mismatch).
// @Tags         ServiceAccounts
// @Accept       json
// @Produce      json
// @Param        id       path      string                true  "Tenant UUID"  format(uuid)
// @Param        request  body      registerRequestBody   true  "Principal registration"
// @Success      200      {object}  principalResponseBody  "already registered — idempotent repeat"
// @Success      201      {object}  principalResponseBody  "created"
// @Failure      400  {object}  ErrorResponse  "invalid_request"
// @Failure      401  {object}  ErrorResponse  "missing_identity_headers"
// @Failure      403  {object}  ErrorResponse  "tenant_path_mismatch"
// @Failure      409  {object}  ErrorResponse  "rotation_in_flight (retryable; the principal row is locked by a concurrent credential write)"
// @Failure      415  {object}  ErrorResponse  "unsupported_media_type"
// @Failure      500  {object}  ErrorResponse
// @Failure      503  {object}  ErrorResponse  "db_unavailable"
// @Security     TenantID && UserID
// @Router       /tenants/{id}/service-accounts [post]
func (h *PrincipalHandler) Register(c *gin.Context) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		writeMissingIdentityHeaders(c)
		return
	}

	var body registerRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		writeInvalidRequest(c, "body", "malformed JSON body")
		return
	}

	// principal_sub's "must be a UUID" check (§10.3) happens here, not in
	// the service layer: the service's RegisterRequest.PrincipalSub is
	// already typed uuid.UUID, so a non-UUID wire value can never reach it
	// as a value to validate — the HTTP layer is where a JSON string is
	// still a string. keycloak_client_id's content check (must equal the
	// frozen platform-automation value) is a value-level business rule and
	// stays in the service layer (§5.1).
	principalSub, err := uuid.Parse(body.PrincipalSub)
	if err != nil {
		writeInvalidRequest(c, "principal_sub", "must be a UUID")
		return
	}

	res, err := h.svc.Register(c.Request.Context(), rc.TenantID, service.RegisterRequest{
		PrincipalSub: principalSub, KeycloakClientID: body.KeycloakClientID,
	}, rc.UserID)
	if err != nil {
		HandleError(c, err)
		return
	}

	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	c.JSON(status, principalResponseBody{
		PrincipalID: res.PrincipalID, TenantID: res.TenantID,
		PrincipalType: string(res.PrincipalType), Status: string(res.Status),
		RecordVersion: res.RecordVersion,
	})
}

type credentialSummaryBody struct {
	Version             int     `json:"version"`
	Status              string  `json:"status"`
	OpenBaoPath         string  `json:"openbao_path"`
	IssuedAt            string  `json:"issued_at"`
	ExpiresAt           *string `json:"expires_at,omitempty"`
	RotationCadenceDays *int    `json:"rotation_cadence_days,omitempty"`
	NextRotationAt      *string `json:"next_rotation_at,omitempty"`
}

type readPrincipalResponseBody struct {
	PrincipalID      uuid.UUID               `json:"principal_id"`
	TenantID         uuid.UUID               `json:"tenant_id"`
	KeycloakClientID string                  `json:"keycloak_client_id"`
	PrincipalType    string                  `json:"principal_type"`
	Status           string                  `json:"status"`
	RecordVersion    int                     `json:"record_version"`
	Credentials      []credentialSummaryBody `json:"credentials"`
}

// Read implements TS-3: GET
// /api/v1/internal/tenants/:id/service-accounts/:principal_id (§5.4) —
// metadata only, never touches OpenBao, never returns a secret (§5.6).
//
// @Summary      TS-3 — Read service-account principal
// @Description  Returns principal metadata and a summary of every credential version (status, OpenBao path, issued/expires timestamps) — never the credential plaintext itself (§5.6). Requires x-user-id = the fixed iam-system principal UUID 00000000-0000-0000-0000-0000000000a1 (domain.SystemPrincipalID; any other value is 401 missing_identity_headers) and x-tenant-id equal to the {id} path segment (else 403 tenant_path_mismatch).
// @Tags         ServiceAccounts
// @Produce      json
// @Param        id            path      string  true  "Tenant UUID"     format(uuid)
// @Param        principal_id  path      string  true  "Principal UUID"  format(uuid)
// @Success      200           {object}  readPrincipalResponseBody
// @Failure      400  {object}  ErrorResponse  "invalid_request"
// @Failure      401  {object}  ErrorResponse  "missing_identity_headers"
// @Failure      403  {object}  ErrorResponse  "tenant_path_mismatch"
// @Failure      404  {object}  ErrorResponse  "principal_not_found"
// @Failure      500  {object}  ErrorResponse
// @Failure      503  {object}  ErrorResponse  "db_unavailable"
// @Security     TenantID && UserID
// @Router       /tenants/{id}/service-accounts/{principal_id} [get]
func (h *PrincipalHandler) Read(c *gin.Context) {
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

	res, err := h.svc.ReadPrincipal(c.Request.Context(), rc.TenantID, principalID)
	if err != nil {
		HandleError(c, err)
		return
	}

	creds := make([]credentialSummaryBody, 0, len(res.Credentials))
	for _, cr := range res.Credentials {
		var expiresAt *string
		if cr.ExpiresAt != nil {
			s := cr.ExpiresAt.UTC().Format(time.RFC3339)
			expiresAt = &s
		}
		var nextRotationAt *string
		if cr.NextRotationAt != nil {
			s := cr.NextRotationAt.UTC().Format(time.RFC3339)
			nextRotationAt = &s
		}
		creds = append(creds, credentialSummaryBody{
			Version: cr.Version, Status: string(cr.Status), OpenBaoPath: cr.OpenBaoPath,
			IssuedAt: cr.IssuedAt.UTC().Format(time.RFC3339), ExpiresAt: expiresAt,
			RotationCadenceDays: cr.RotationCadenceDays, NextRotationAt: nextRotationAt,
		})
	}

	c.JSON(http.StatusOK, readPrincipalResponseBody{
		PrincipalID: res.PrincipalID, TenantID: res.TenantID, KeycloakClientID: res.KeycloakClientID,
		PrincipalType: string(res.PrincipalType), Status: string(res.Status), RecordVersion: res.RecordVersion,
		Credentials: creds,
	})
}

// FindBySub implements TS-5: GET
// /api/v1/internal/tenants/:id/service-accounts?principal_sub=<uuid>
// (AUTH-9) — a query-param lookup on the existing collection path rather
// than a new path segment, since a static "by-sub" segment at the same
// tree position as the existing GET .../service-accounts/:principal_id
// route would conflict in gin's router (a static segment and a named
// parameter can't share one position). Deliberately returns only
// identity/status (principalResponseBody), never credential metadata —
// callers checking "is this a service account" have no need for it.
//
// @Summary      TS-5 — Find service-account principal by Keycloak sub
// @Description  Looks up a principal by its Keycloak sub rather than this service's own internal id (AUTH-9) — the signal another service needs to answer "does this subject resolve to a service_account-typed Keycloak principal" without ever generating principal_sub itself (TS-INV-1). Requires x-user-id = the fixed iam-system principal UUID 00000000-0000-0000-0000-0000000000a1 (domain.SystemPrincipalID; any other value is 401 missing_identity_headers) and x-tenant-id equal to the {id} path segment (else 403 tenant_path_mismatch).
// @Tags         ServiceAccounts
// @Produce      json
// @Param        id             path      string  true  "Tenant UUID"          format(uuid)
// @Param        principal_sub  query     string  true  "Keycloak sub UUID"    format(uuid)
// @Success      200            {object}  principalResponseBody
// @Failure      400  {object}  ErrorResponse  "invalid_request"
// @Failure      401  {object}  ErrorResponse  "missing_identity_headers"
// @Failure      403  {object}  ErrorResponse  "tenant_path_mismatch"
// @Failure      404  {object}  ErrorResponse  "principal_not_found"
// @Failure      500  {object}  ErrorResponse
// @Failure      503  {object}  ErrorResponse  "db_unavailable"
// @Security     TenantID && UserID
// @Router       /tenants/{id}/service-accounts [get]
func (h *PrincipalHandler) FindBySub(c *gin.Context) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		writeMissingIdentityHeaders(c)
		return
	}

	principalSub, err := uuid.Parse(c.Query("principal_sub"))
	if err != nil {
		writeInvalidRequest(c, "principal_sub", "must be a UUID")
		return
	}

	res, err := h.svc.FindPrincipalBySub(c.Request.Context(), rc.TenantID, principalSub)
	if err != nil {
		HandleError(c, err)
		return
	}

	c.JSON(http.StatusOK, principalResponseBody{
		PrincipalID: res.PrincipalID, TenantID: res.TenantID,
		PrincipalType: string(res.PrincipalType), Status: string(res.Status), RecordVersion: res.RecordVersion,
	})
}

type automationPrincipalResponseBody struct {
	PrincipalID      uuid.UUID `json:"principal_id"`
	TenantID         uuid.UUID `json:"tenant_id"`
	PrincipalSub     uuid.UUID `json:"principal_sub"`
	KeycloakClientID string    `json:"keycloak_client_id"`
	PrincipalType    string    `json:"principal_type"`
	Status           string    `json:"status"`
	RecordVersion    int       `json:"record_version"`
}

// ReadPlatformAutomation implements TS-6: GET
// /api/v1/internal/tenants/:id/service-accounts/platform-automation
// (TS-D17) — the reverse of TS-5: resolves tenant T's automation subject
// from the tenant id alone. Addressed by the frozen principal name (the
// same segment the EXT-6 JWKS route already uses) rather than a
// principal_id the caller would first have to learn. Identity only, never
// credential metadata.
//
// @Summary      TS-6 — Read the tenant's platform-automation principal
// @Description  Returns the tenant's platform-automation principal, including its Keycloak sub (principal_sub) — the subject a caller names as the acting principal (e.g. Workflow's connector callbacks). principal_sub is stable across credential rotation but changes when RP-3 (convert) or RP-4 (revert-conversion) re-mints the client in another realm; principal_id is stable across both. Never returns credential metadata or material. Requires x-user-id = the fixed iam-system principal UUID 00000000-0000-0000-0000-0000000000a1 (domain.SystemPrincipalID; any other value is 401 missing_identity_headers) and x-tenant-id equal to the {id} path segment (else 403 tenant_path_mismatch).
// @Tags         ServiceAccounts
// @Produce      json
// @Param        id   path      string  true  "Tenant UUID"  format(uuid)
// @Success      200  {object}  automationPrincipalResponseBody
// @Failure      400  {object}  ErrorResponse  "invalid_request"
// @Failure      401  {object}  ErrorResponse  "missing_identity_headers"
// @Failure      403  {object}  ErrorResponse  "tenant_path_mismatch"
// @Failure      404  {object}  ErrorResponse  "principal_not_found"
// @Failure      500  {object}  ErrorResponse
// @Failure      503  {object}  ErrorResponse  "db_unavailable"
// @Security     TenantID && UserID
// @Router       /tenants/{id}/service-accounts/platform-automation [get]
func (h *PrincipalHandler) ReadPlatformAutomation(c *gin.Context) {
	rc, ok := requestctx.FromContext(c.Request.Context())
	if !ok {
		writeMissingIdentityHeaders(c)
		return
	}

	res, err := h.svc.ReadPlatformAutomation(c.Request.Context(), rc.TenantID)
	if err != nil {
		HandleError(c, err)
		return
	}

	c.JSON(http.StatusOK, automationPrincipalResponseBody{
		PrincipalID: res.PrincipalID, TenantID: res.TenantID, PrincipalSub: res.PrincipalSub,
		KeycloakClientID: res.KeycloakClientID, PrincipalType: string(res.PrincipalType),
		Status: string(res.Status), RecordVersion: res.RecordVersion,
	})
}
