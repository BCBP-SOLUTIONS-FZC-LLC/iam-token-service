package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// treeSecrets is a port.SecretStore over a fixed folder tree: List returns
// the scripted children of a prefix (sub-folders suffixed "/"), Delete
// records each leaf. listErr / deleteErr fail the call for one exact path.
// Only List and Delete are implemented; any other method panics via the nil
// embedded interface, proving EraseSecretMaterial never calls it.
type treeSecrets struct {
	port.SecretStore
	tree      map[string][]string
	listed    []string
	deleted   []string
	listErr   map[string]error
	deleteErr map[string]error
	deadlines []bool
}

func (s *treeSecrets) List(ctx context.Context, prefix string) ([]string, error) {
	_, ok := ctx.Deadline()
	s.deadlines = append(s.deadlines, ok)
	s.listed = append(s.listed, prefix)
	if err := s.listErr[prefix]; err != nil {
		return nil, err
	}
	return s.tree[prefix], nil
}

func (s *treeSecrets) Delete(ctx context.Context, path string) error {
	_, ok := ctx.Deadline()
	s.deadlines = append(s.deadlines, ok)
	if err := s.deleteErr[path]; err != nil {
		return err
	}
	s.deleted = append(s.deleted, path)
	return nil
}

// The listed paths go first, then the walk recurses through "/"-suffixed
// folders, deletes every leaf, and stops descending below the frozen
// <tenant>/<client>/v<n> layout's depth bound.
func TestEraseSecretMaterial_PathsThenSubtreeAndStopsBelowMaxDepth(t *testing.T) {
	// root/a/b/c/d/e/: depth 0..5; the folder at depth 5 must never be listed.
	s := &treeSecrets{tree: map[string][]string{
		"root":           {"leaf0", "a/"},
		"root/a":         {"leaf1", "b/"},
		"root/a/b":       {"c/"},
		"root/a/b/c":     {"d/"},
		"root/a/b/c/d":   {"leaf4", "e/"},
		"root/a/b/c/d/e": {"too-deep"},
	}}

	require.NoError(t, service.EraseSecretMaterial(context.Background(), s, []string{"old/v1"}, "root", 0))

	assert.Equal(t, []string{"old/v1", "root/leaf0", "root/a/leaf1", "root/a/b/c/d/leaf4"}, s.deleted)
	assert.NotContains(t, s.listed, "root/a/b/c/d/e", "depth > maxSubtreeDepth is not walked")
	for _, hasDeadline := range s.deadlines {
		assert.False(t, hasDeadline, "perCall 0 leaves the calls unbounded (the purge cascade)")
	}
}

func TestEraseSecretMaterial_PerCallBound(t *testing.T) {
	s := &treeSecrets{tree: map[string][]string{"root": {"v1"}}}
	require.NoError(t, service.EraseSecretMaterial(context.Background(), s, []string{"x"}, "root", time.Second))
	require.Len(t, s.deadlines, 3, "delete x, list root, delete root/v1")
	for _, hasDeadline := range s.deadlines {
		assert.True(t, hasDeadline, "TS-7 bounds every OpenBao call it makes under the principal lock")
	}
}

func TestEraseSecretMaterial_PathDeleteErrorStopsBeforeSubtree(t *testing.T) {
	boom := errors.New("delete failed")
	s := &treeSecrets{deleteErr: map[string]error{"old/v1": boom}}
	assert.ErrorIs(t, service.EraseSecretMaterial(context.Background(), s, []string{"old/v1"}, "root", 0), boom)
	assert.Empty(t, s.listed)
}

func TestEraseSecretMaterial_ListErrorPropagates(t *testing.T) {
	boom := errors.New("list failed")
	s := &treeSecrets{listErr: map[string]error{"root": boom}}
	assert.ErrorIs(t, service.EraseSecretMaterial(context.Background(), s, nil, "root", 0), boom)
}

// A failure in a nested folder aborts the whole walk: siblings after it are
// not deleted, so the caller's transaction rolls back and a retry starts
// from the top.
func TestEraseSecretMaterial_NestedListErrorPropagates(t *testing.T) {
	boom := errors.New("nested list failed")
	s := &treeSecrets{
		tree:    map[string][]string{"root": {"a/", "after"}},
		listErr: map[string]error{"root/a": boom},
	}
	assert.ErrorIs(t, service.EraseSecretMaterial(context.Background(), s, nil, "root", 0), boom)
	assert.Empty(t, s.deleted)
}

func TestEraseSecretMaterial_LeafDeleteErrorPropagates(t *testing.T) {
	boom := errors.New("delete failed")
	s := &treeSecrets{
		tree:      map[string][]string{"root": {"x", "y"}},
		deleteErr: map[string]error{"root/x": boom},
	}
	assert.ErrorIs(t, service.EraseSecretMaterial(context.Background(), s, nil, "root", 0), boom)
	assert.Empty(t, s.deleted, "the walk stops at the first failed delete")
}
