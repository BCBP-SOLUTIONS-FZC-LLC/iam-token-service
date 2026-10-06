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
// embedded interface, proving deleteSubtree never calls it.
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

// The walk recurses through "/"-suffixed folders, deletes every leaf, and
// stops descending below maxSubtreeDepth (anything deeper than the frozen
// <tenant>/<client>/v<n> layout is not this service's material).
func TestDeleteSubtree_RecursesAndStopsBelowMaxDepth(t *testing.T) {
	// root/a/b/c/d/e/: depth 0..5; the folder at depth 5 must never be listed.
	s := &treeSecrets{tree: map[string][]string{
		"root":           {"leaf0", "a/"},
		"root/a":         {"leaf1", "b/"},
		"root/a/b":       {"c/"},
		"root/a/b/c":     {"d/"},
		"root/a/b/c/d":   {"leaf4", "e/"},
		"root/a/b/c/d/e": {"too-deep"},
	}}
	c := &OffboardingConsumer{secrets: s}

	require.NoError(t, c.deleteSubtree(context.Background(), "root", 0))

	assert.Equal(t, []string{"root/leaf0", "root/a/leaf1", "root/a/b/c/d/leaf4"}, s.deleted)
	assert.NotContains(t, s.listed, "root/a/b/c/d/e", "depth > maxSubtreeDepth is not walked")
	assert.NotContains(t, s.deleted, "root/a/b/c/d/e/too-deep")
}

func TestDeleteSubtree_ListErrorPropagates(t *testing.T) {
	boom := errors.New("list failed")
	s := &treeSecrets{listErr: map[string]error{"root": boom}}
	c := &OffboardingConsumer{secrets: s}
	assert.ErrorIs(t, c.deleteSubtree(context.Background(), "root", 0), boom)
}

// A failure in a nested folder aborts the whole walk: siblings after it are
// not deleted, so the cascade's transaction rolls back and redelivery
// retries from the top.
func TestDeleteSubtree_NestedListErrorPropagates(t *testing.T) {
	boom := errors.New("nested list failed")
	s := &treeSecrets{
		tree:    map[string][]string{"root": {"a/", "after"}},
		listErr: map[string]error{"root/a": boom},
	}
	c := &OffboardingConsumer{secrets: s}
	assert.ErrorIs(t, c.deleteSubtree(context.Background(), "root", 0), boom)
	assert.Empty(t, s.deleted)
}

func TestDeleteSubtree_LeafDeleteErrorPropagates(t *testing.T) {
	boom := errors.New("delete failed")
	s := &treeSecrets{
		tree:      map[string][]string{"root": {"x", "y"}},
		deleteErr: map[string]error{"root/x": boom},
	}
	c := &OffboardingConsumer{secrets: s}
	assert.ErrorIs(t, c.deleteSubtree(context.Background(), "root", 0), boom)
	assert.Empty(t, s.deleted, "the walk stops at the first failed delete")
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
