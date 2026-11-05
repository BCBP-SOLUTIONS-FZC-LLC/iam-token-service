package service

import (
	"context"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/pkg/traceutil"
)

// tracer names every span this package starts. OpenTelemetry via
// platform-gincommon middleware installs the TracerProvider (cmd/*'s
// gincommon.InitTracingWithConfig) — this package only needs a Tracer handle,
// obtained lazily via otel.Tracer so unit tests that never install a
// provider still get a valid no-op tracer rather than a panic.
var tracer = otel.Tracer("iam-token-service/credential")

// IssueOrRotate is the exported TS-1 entry point: starts the
// "credential.issue_rotate" span (§11.3, attrs tenant_id/principal_id/
// version/op) around issueOrRotate's business logic.
func (s *CredentialService) IssueOrRotate(ctx context.Context, tenantID, principalID uuid.UUID, req IssueOrRotateRequest, actor uuid.UUID) (*IssueOrRotateResult, error) {
	ctx, span := tracer.Start(ctx, "credential.issue_rotate", oteltrace.WithAttributes(
		attribute.String("tenant_id", tenantID.String()),
		attribute.String("principal_id", principalID.String()),
	))
	defer span.End()

	result, err := s.issueOrRotate(ctx, tenantID, principalID, req, actor)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	op := "issue"
	if result.ExpiresPriorAt != nil {
		op = "rotate"
	}
	span.SetAttributes(
		attribute.Int("version", result.Version),
		attribute.String("op", op),
		attribute.Bool("replayed", result.Replayed),
	)
	return result, nil
}

// Revoke is the exported TS-2 entry point: starts the "credential.revoke"
// span (§11.3, attrs tenant_id/principal_id/version/op) around revoke's
// business logic.
func (s *CredentialService) Revoke(ctx context.Context, tenantID, principalID uuid.UUID, version int, actor uuid.UUID) (*RevokeResult, error) {
	ctx, span := tracer.Start(ctx, "credential.revoke", oteltrace.WithAttributes(
		attribute.String("tenant_id", tenantID.String()),
		attribute.String("principal_id", principalID.String()),
		attribute.Int("version", version),
		attribute.String("op", "revoke"),
	))
	defer span.End()

	result, err := s.revoke(ctx, tenantID, principalID, version, actor)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	return result, nil
}

// withTraceID adds trace_id to fields if the context carries a valid span.
// Delegates to pkg/traceutil.WithTraceID so the implementation is shared
// with other packages (L-10).
func withTraceID(ctx context.Context, fields map[string]interface{}) map[string]interface{} {
	// pkg/traceutil.WithTraceID accepts map[string]any; map[string]interface{}
	// is the same type, so cast is safe.
	return traceutil.WithTraceID(ctx, fields)
}

// RevokePrincipal is the exported TS-7 entry point: starts the
// "principal.revoke" span (§11.3, attrs tenant_id/op) around
// revokePrincipal's business logic.
func (s *CredentialService) RevokePrincipal(ctx context.Context, tenantID, actor uuid.UUID) (*RevokePrincipalResult, error) {
	ctx, span := tracer.Start(ctx, "principal.revoke", oteltrace.WithAttributes(
		attribute.String("tenant_id", tenantID.String()),
		attribute.String("op", "revoke_principal"),
	))
	defer span.End()

	result, err := s.revokePrincipal(ctx, tenantID, actor)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	span.SetAttributes(
		attribute.String("principal_id", result.PrincipalID.String()),
		attribute.Int("newly_revoked", result.NewlyRevoked),
	)
	return result, nil
}
