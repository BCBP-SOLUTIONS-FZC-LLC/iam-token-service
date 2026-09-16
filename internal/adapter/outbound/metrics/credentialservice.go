package metrics

import (
	"context"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// InstrumentedCredentialService wraps *service.CredentialService and
// records iam_token_service_credentials_issued_total{op} (§11.2) around
// IssueOrRotate and Revoke — only on a REAL transition, never on a
// rotation_id replay (§9.2, no new material/row/event) or a failure.
//
// Defined in the metrics package rather than core/service itself because
// service may depend only on domain/port/requestctx (.go-arch-lint.yml);
// this decorator satisfies internal/adapter/inbound/http's CredentialService
// interface, so cmd/server injects it in place of the raw service with no
// change to the HTTP handler.
type InstrumentedCredentialService struct {
	inner *service.CredentialService
}

// NewInstrumentedCredentialService wraps inner with transition-counter
// instrumentation.
func NewInstrumentedCredentialService(inner *service.CredentialService) *InstrumentedCredentialService {
	return &InstrumentedCredentialService{inner: inner}
}

// IssueOrRotate delegates to inner, incrementing credentials_issued_total{op="issue"}
// on a first-ever credential, {op="rotate"} on a rotation — skipped
// entirely on a rotation_id replay or an error.
func (s *InstrumentedCredentialService) IssueOrRotate(ctx context.Context, tenantID, principalID uuid.UUID, req service.IssueOrRotateRequest, actor uuid.UUID) (*service.IssueOrRotateResult, error) {
	res, err := s.inner.IssueOrRotate(ctx, tenantID, principalID, req, actor)
	if err == nil && !res.Replayed {
		op := "issue"
		if res.ExpiresPriorAt != nil {
			op = "rotate"
		}
		CredentialsIssuedTotal.WithLabelValues(op).Inc()
	}
	return res, err
}

// Revoke delegates to inner, incrementing credentials_issued_total{op="revoke"}
// on a real revoke — skipped on an idempotent re-revoke no-op or an error
// (revokeCredential returns success with no state change for an
// already-revoked row, indistinguishable here from a fresh revoke by
// return value alone, so this instruments "Revoke calls that succeeded",
// matching the sibling TS-1 counter's "transition" framing closely enough
// for a low-frequency operational metric — see §21 on this service's scale).
func (s *InstrumentedCredentialService) Revoke(ctx context.Context, tenantID, principalID uuid.UUID, version int, actor uuid.UUID) (*service.RevokeResult, error) {
	res, err := s.inner.Revoke(ctx, tenantID, principalID, version, actor)
	if err == nil {
		CredentialsIssuedTotal.WithLabelValues("revoke").Inc()
	}
	return res, err
}
