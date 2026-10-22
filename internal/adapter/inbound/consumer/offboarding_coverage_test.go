package consumer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// treeSecrets is a port.SecretStore over a fixed folder tree: List returns
// the scripted children of a prefix (sub-folders suffixed "/"), Delete
// records each leaf. listErr / deleteErr fail the call for one exact path.
// Only List and Delete are implemented; any other method panics via the nil
// embedded interface, proving the cascade's erasure never calls it. The
// walk itself is unit-tested with service.EraseSecretMaterial
// (test/unit/service/secret_erasure_test.go).
type treeSecrets struct {
	port.SecretStore
	tree      map[string][]string
	listed    []string
	deleted   []string
	listErr   map[string]error
	deleteErr map[string]error
}

func (s *treeSecrets) List(_ context.Context, prefix string) ([]string, error) {
	s.listed = append(s.listed, prefix)
	if err := s.listErr[prefix]; err != nil {
		return nil, err
	}
	return s.tree[prefix], nil
}

func (s *treeSecrets) Delete(_ context.Context, path string) error {
	if err := s.deleteErr[path]; err != nil {
		return err
	}
	s.deleted = append(s.deleted, path)
	return nil
}

// stubPrincipals implements only what cascade calls before the subtree walk.
type stubPrincipals struct {
	port.PrincipalRepository
	deleteCalled bool
}

func (p *stubPrincipals) LockByTenant(context.Context, uuid.UUID) ([]*domain.ServiceAccountPrincipal, error) {
	return nil, nil
}

func (p *stubPrincipals) DeleteByTenant(context.Context, uuid.UUID) error {
	p.deleteCalled = true
	return nil
}

// A failed walk of the tenant's OpenBao subtree fails the cascade BEFORE the
// principal rows are deleted (§15.2: once they are gone, the reconciler can
// never find the leftover material).
func TestCascade_SubtreeErrorStopsBeforePrincipalDelete(t *testing.T) {
	tenantID := uuid.New()
	boom := errors.New("openbao list down")
	s := &treeSecrets{listErr: map[string]error{domain.OpenBaoTenantPrefix(tenantID): boom}}
	p := &stubPrincipals{}
	c := &OffboardingConsumer{principals: p, secrets: s}

	err := c.cascade(context.Background(), tenantID)

	require.ErrorIs(t, err, boom)
	assert.False(t, p.deleteCalled, "DeleteByTenant must not run after a failed material erasure")
}

// withTraceID allocates the field map when the caller passed none, so a
// trace id is never dropped.
func TestWithTraceID_NilFieldsGetTraceID(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01, 0x02, 0x03}, SpanID: trace.SpanID{0x04}, TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	fields := withTraceID(ctx, nil)

	require.NotNil(t, fields)
	assert.Equal(t, sc.TraceID().String(), fields["trace_id"])
	assert.True(t, strings.HasPrefix(fields["trace_id"].(string), "010203"))
	assert.Nil(t, withTraceID(context.Background(), nil), "no span, no allocation")
}
