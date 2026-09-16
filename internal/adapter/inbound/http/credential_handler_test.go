package http

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

func newCredentialTestRouter(svc CredentialService) *gin.Engine {
	h := NewCredentialHandler(svc)
	return newTenantScopedTestRouter(func(g *gin.RouterGroup) {
		g.POST("/service-accounts/:principal_id/credentials", h.IssueOrRotate)
		g.POST("/service-accounts/:principal_id/credentials/:version/revoke", h.Revoke)
	})
}

func TestCredentialHandler_IssueOrRotate(t *testing.T) {
	tenantID := uuid.New()
	principalID := uuid.New()
	path := "/tenants/" + tenantID.String() + "/service-accounts/" + principalID.String() + "/credentials"
	rotationID := uuid.New()

	t.Run("201 issued", func(t *testing.T) {
		svc := &fakeCredentialService{issueResult: &service.IssueOrRotateResult{
			Version: 1, Secret: "s3cr3t", OpenBaoPath: "iam/serviceaccount/x/y/v1", RecordVersion: 1,
		}}
		r := newCredentialTestRouter(svc)
		var body issueOrRotateResponseBody
		rec := doJSON(t, r, http.MethodPost, path, sysHeaders(tenantID),
			issueOrRotateRequestBody{RotationID: rotationID.String()}, &body)
		require.Equal(t, http.StatusCreated, rec.Code)
		assert.Equal(t, "s3cr3t", body.Secret)
		assert.Nil(t, body.ExpiresPriorAt)
	})

	t.Run("201 rotated with expires_prior_at", func(t *testing.T) {
		expiresPrior := time.Now().UTC()
		svc := &fakeCredentialService{issueResult: &service.IssueOrRotateResult{
			Version: 2, Secret: "s3cr3t2", OpenBaoPath: "iam/serviceaccount/x/y/v2",
			ExpiresPriorAt: &expiresPrior, RecordVersion: 1,
		}}
		r := newCredentialTestRouter(svc)
		var body issueOrRotateResponseBody
		rec := doJSON(t, r, http.MethodPost, path, sysHeaders(tenantID),
			issueOrRotateRequestBody{RotationID: rotationID.String()}, &body)
		require.Equal(t, http.StatusCreated, rec.Code)
		require.NotNil(t, body.ExpiresPriorAt)
		assert.Equal(t, expiresPrior.Format(time.RFC3339), *body.ExpiresPriorAt)
	})

	t.Run("200 on rotation_id replay", func(t *testing.T) {
		svc := &fakeCredentialService{issueResult: &service.IssueOrRotateResult{
			Version: 1, Secret: "s3cr3t", Replayed: true,
		}}
		r := newCredentialTestRouter(svc)
		rec := doJSON(t, r, http.MethodPost, path, sysHeaders(tenantID),
			issueOrRotateRequestBody{RotationID: rotationID.String()}, nil)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("401 missing identity headers", func(t *testing.T) {
		r := newCredentialTestRouter(&fakeCredentialService{})
		rec := doRequest(t, r, http.MethodPost, path, map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("400 non-UUID principal_id", func(t *testing.T) {
		r := newCredentialTestRouter(&fakeCredentialService{})
		badPath := "/tenants/" + tenantID.String() + "/service-accounts/not-a-uuid/credentials"
		rec := doJSON(t, r, http.MethodPost, badPath, sysHeaders(tenantID), issueOrRotateRequestBody{RotationID: rotationID.String()}, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "principal_id", er.Details["field"])
	})

	t.Run("400 malformed JSON body", func(t *testing.T) {
		r := newCredentialTestRouter(&fakeCredentialService{})
		rec := doRequest(t, r, http.MethodPost, path, sysHeaders(tenantID), []byte(`{not-json`))
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("400 non-UUID rotation_id", func(t *testing.T) {
		r := newCredentialTestRouter(&fakeCredentialService{})
		rec := doJSON(t, r, http.MethodPost, path, sysHeaders(tenantID), issueOrRotateRequestBody{RotationID: "not-a-uuid"}, nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "rotation_id", er.Details["field"])
	})

	t.Run("omitted rotation_id passes through as uuid.Nil", func(t *testing.T) {
		var gotReq service.IssueOrRotateRequest
		svc := &fakeCredentialService{issueFn: func(_ context.Context, _, _ uuid.UUID, req service.IssueOrRotateRequest, _ uuid.UUID) (*service.IssueOrRotateResult, error) {
			gotReq = req
			return &service.IssueOrRotateResult{Version: 1, Secret: "s"}, nil
		}}
		r := newCredentialTestRouter(svc)
		rec := doJSON(t, r, http.MethodPost, path, sysHeaders(tenantID), issueOrRotateRequestBody{}, nil)
		require.Equal(t, http.StatusCreated, rec.Code)
		assert.Equal(t, uuid.Nil, gotReq.RotationID)
	})

	t.Run("omitted overlap_seconds defaults; explicit 0 is a hard cutover", func(t *testing.T) {
		var gotReqs []service.IssueOrRotateRequest
		svc := &fakeCredentialService{issueFn: func(_ context.Context, _, _ uuid.UUID, req service.IssueOrRotateRequest, _ uuid.UUID) (*service.IssueOrRotateResult, error) {
			gotReqs = append(gotReqs, req)
			return &service.IssueOrRotateResult{Version: 1, Secret: "s"}, nil
		}}
		r := newCredentialTestRouter(svc)

		doJSON(t, r, http.MethodPost, path, sysHeaders(tenantID), issueOrRotateRequestBody{RotationID: rotationID.String()}, nil)
		require.Len(t, gotReqs, 1)
		assert.Equal(t, domain.DefaultOverlapSeconds, gotReqs[0].OverlapSeconds)

		zero := 0
		doJSON(t, r, http.MethodPost, path, sysHeaders(tenantID), issueOrRotateRequestBody{RotationID: uuid.New().String(), OverlapSeconds: &zero}, nil)
		require.Len(t, gotReqs, 2)
		assert.Equal(t, 0, gotReqs[1].OverlapSeconds)
	})

	t.Run("maps every domain error from the service", func(t *testing.T) {
		cases := []struct {
			name       string
			err        error
			wantStatus int
		}{
			{"principal_not_found", domain.NewError(domain.ErrPrincipalNotFound, "x"), http.StatusNotFound},
			{"rotation_in_flight", domain.NewError(domain.ErrRotationInFlight, "x"), http.StatusConflict},
			{"optimistic_lock_conflict", domain.NewError(domain.ErrOptimisticLockConflict, "x"), http.StatusConflict},
			{"principal_revoked", domain.NewError(domain.ErrPrincipalRevoked, "x"), http.StatusUnprocessableEntity},
			{"secret_store_unavailable", domain.NewError(domain.ErrSecretStoreUnavailable, "x"), http.StatusBadGateway},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				svc := &fakeCredentialService{issueErr: tc.err}
				r := newCredentialTestRouter(svc)
				rec := doJSON(t, r, http.MethodPost, path, sysHeaders(tenantID), issueOrRotateRequestBody{RotationID: uuid.New().String()}, nil)
				assert.Equal(t, tc.wantStatus, rec.Code)
			})
		}
	})
}

func TestCredentialHandler_Revoke(t *testing.T) {
	tenantID := uuid.New()
	principalID := uuid.New()
	base := "/tenants/" + tenantID.String() + "/service-accounts/" + principalID.String() + "/credentials"

	t.Run("200", func(t *testing.T) {
		revokedAt := time.Now().UTC()
		svc := &fakeCredentialService{revokeResult: &service.RevokeResult{Version: 1, Status: domain.CredentialStatusRevoked, RevokedAt: revokedAt}}
		r := newCredentialTestRouter(svc)
		var body revokeResponseBody
		rec := doJSON(t, r, http.MethodPost, base+"/1/revoke", sysHeaders(tenantID), nil, &body)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "revoked", body.Status)
		assert.Equal(t, "caller_responsibility", body.KeycloakInvalidation)
	})

	t.Run("401 missing identity headers", func(t *testing.T) {
		r := newCredentialTestRouter(&fakeCredentialService{})
		rec := doRequest(t, r, http.MethodPost, base+"/1/revoke", nil, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("400 non-UUID principal_id", func(t *testing.T) {
		r := newCredentialTestRouter(&fakeCredentialService{})
		badPath := "/tenants/" + tenantID.String() + "/service-accounts/not-a-uuid/credentials/1/revoke"
		rec := doRequest(t, r, http.MethodPost, badPath, sysHeaders(tenantID), nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("400 non-numeric version", func(t *testing.T) {
		r := newCredentialTestRouter(&fakeCredentialService{})
		rec := doRequest(t, r, http.MethodPost, base+"/abc/revoke", sysHeaders(tenantID), nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		er := decodeErrorBody(t, rec)
		assert.Equal(t, "version", er.Details["field"])
	})

	t.Run("400 non-positive version", func(t *testing.T) {
		r := newCredentialTestRouter(&fakeCredentialService{})
		rec := doRequest(t, r, http.MethodPost, base+"/0/revoke", sysHeaders(tenantID), nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("maps every domain error from the service", func(t *testing.T) {
		cases := []struct {
			name       string
			err        error
			wantStatus int
		}{
			{"principal_not_found", domain.NewError(domain.ErrPrincipalNotFound, "x"), http.StatusNotFound},
			{"optimistic_lock_conflict", domain.NewError(domain.ErrOptimisticLockConflict, "x"), http.StatusConflict},
			{"secret_store_unavailable", domain.NewError(domain.ErrSecretStoreUnavailable, "x"), http.StatusBadGateway},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				svc := &fakeCredentialService{revokeErr: tc.err}
				r := newCredentialTestRouter(svc)
				rec := doRequest(t, r, http.MethodPost, base+"/1/revoke", sysHeaders(tenantID), nil)
				assert.Equal(t, tc.wantStatus, rec.Code)
			})
		}
	})
}

// TestCredentialHandler_DefensiveMissingRequestContext mirrors
// TestPrincipalHandler_DefensiveMissingRequestContext for CredentialHandler.
func TestCredentialHandler_DefensiveMissingRequestContext(t *testing.T) {
	h := NewCredentialHandler(&fakeCredentialService{})
	r := gin.New()
	r.POST("/service-accounts/:principal_id/credentials", h.IssueOrRotate)
	r.POST("/service-accounts/:principal_id/credentials/:version/revoke", h.Revoke)

	pid := uuid.New().String()
	rec := doRequest(t, r, http.MethodPost, "/service-accounts/"+pid+"/credentials", map[string]string{"Content-Type": "application/json"}, []byte(`{}`))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = doRequest(t, r, http.MethodPost, "/service-accounts/"+pid+"/credentials/1/revoke", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
