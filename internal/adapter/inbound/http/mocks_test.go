package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// ─────────────────────────────────────────────────────────────────────────
// Hand-written fakes — no mocking framework is used elsewhere in this repo.
// ─────────────────────────────────────────────────────────────────────────

// fakePrincipalService lets each test script canned results/errors per call
// rather than modeling real business logic (that's test/unit/service's job)
// — this package only needs to verify HTTP binding/status/error-mapping.
type fakePrincipalService struct {
	registerResult *service.RegisterResult
	registerErr    error
	registerFn     func(ctx context.Context, tenantID uuid.UUID, req service.RegisterRequest, actor uuid.UUID) (*service.RegisterResult, error)

	readResult *service.ReadPrincipalResult
	readErr    error

	findBySubResult *service.FindBySubResult
	findBySubErr    error
}

func (f *fakePrincipalService) Register(ctx context.Context, tenantID uuid.UUID, req service.RegisterRequest, actor uuid.UUID) (*service.RegisterResult, error) {
	if f.registerFn != nil {
		return f.registerFn(ctx, tenantID, req, actor)
	}
	return f.registerResult, f.registerErr
}

func (f *fakePrincipalService) ReadPrincipal(_ context.Context, _, _ uuid.UUID) (*service.ReadPrincipalResult, error) {
	return f.readResult, f.readErr
}

func (f *fakePrincipalService) FindPrincipalBySub(_ context.Context, _, _ uuid.UUID) (*service.FindBySubResult, error) {
	return f.findBySubResult, f.findBySubErr
}

var _ PrincipalService = (*fakePrincipalService)(nil)

// fakeCredentialService satisfies the CredentialService interface declared
// in credential_handler.go.
type fakeCredentialService struct {
	issueResult *service.IssueOrRotateResult
	issueErr    error
	issueFn     func(ctx context.Context, tenantID, principalID uuid.UUID, req service.IssueOrRotateRequest, actor uuid.UUID) (*service.IssueOrRotateResult, error)

	revokeResult *service.RevokeResult
	revokeErr    error
}

func (f *fakeCredentialService) IssueOrRotate(ctx context.Context, tenantID, principalID uuid.UUID, req service.IssueOrRotateRequest, actor uuid.UUID) (*service.IssueOrRotateResult, error) {
	if f.issueFn != nil {
		return f.issueFn(ctx, tenantID, principalID, req, actor)
	}
	return f.issueResult, f.issueErr
}

func (f *fakeCredentialService) Revoke(_ context.Context, _, _ uuid.UUID, _ int, _ uuid.UUID) (*service.RevokeResult, error) {
	return f.revokeResult, f.revokeErr
}

var _ CredentialService = (*fakeCredentialService)(nil)

// fakePinger reports healthErr as the result of Health (nil = healthy).
type fakePinger struct{ healthErr error }

func (f fakePinger) Health(context.Context) error { return f.healthErr }

var _ Pinger = fakePinger{}

// fakeLogger records every call — used to assert HandleError's unhandled-500
// logging branch fires (or doesn't).
type fakeLogger struct {
	debugCalls, infoCalls, warnCalls, errorCalls []map[string]any
}

func (f *fakeLogger) Debug(_ string, fields map[string]any) {
	f.debugCalls = append(f.debugCalls, fields)
}
func (f *fakeLogger) Info(_ string, fields map[string]any) { f.infoCalls = append(f.infoCalls, fields) }
func (f *fakeLogger) Warn(_ string, fields map[string]any) { f.warnCalls = append(f.warnCalls, fields) }
func (f *fakeLogger) Error(_ string, fields map[string]any) {
	f.errorCalls = append(f.errorCalls, fields)
}

// ─────────────────────────────────────────────────────────────────────────
// Shared HTTP test helpers.
// ─────────────────────────────────────────────────────────────────────────

// sysHeaders returns a valid iam-system-principal header set scoped to
// tenantID — the only accepted caller shape on /api/v1/internal/* (§5.2).
func sysHeaders(tenantID uuid.UUID) map[string]string {
	return map[string]string{
		"x-user-id":      "00000000-0000-0000-0000-0000000000a1", // domain.SystemPrincipalID
		"x-tenant-id":    tenantID.String(),
		"x-tenant-roles": "iam-system",
		"Content-Type":   "application/json",
	}
}

// doRequest fires req through h and returns the raw recorder.
func doRequest(t *testing.T, h http.Handler, method, path string, headers map[string]string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// doJSON marshals reqBody (nil for no body), fires the request, and decodes
// the response into out (nil to skip decoding).
func doJSON(t *testing.T, h http.Handler, method, path string, headers map[string]string, reqBody, out any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if reqBody != nil {
		var err error
		raw, err = json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
	}
	rec := doRequest(t, h, method, path, headers, raw)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode response body: %v, status=%d, body=%s", err, rec.Code, rec.Body.String())
		}
	}
	return rec
}

// decodeErrorBody decodes rec's body as ErrorResponse for §17-shape assertions.
func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var er ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode error body: %v, body=%s", err, rec.Body.String())
	}
	return er
}
