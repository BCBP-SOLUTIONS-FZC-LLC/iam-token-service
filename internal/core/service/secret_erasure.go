package service

import (
	"context"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// maxSubtreeDepth bounds EraseSecretMaterial's recursive walk: the frozen
// layout is <tenant>/<keycloak_client_id>/v<n> (§6.3), so anything deeper is
// not ours.
const maxSubtreeDepth = 4

// EraseSecretMaterial is the one material-erasure step shared by the two
// paths that retire a whole principal: the TenantMembershipsPurged cascade
// (§8.4) and TS-7 (§5.4). Both must leave no material behind, and keeping it
// in one place stops them drifting apart.
//
// It deletes every path in paths (the committed rows' material, which may
// sit under an older keycloak_client_id after an RP-3/RP-4 re-mint), then
// every leaf under subtree (material a failed or racing TS-1 wrote but never
// committed has no row, so the path list cannot see it). The caller holds the
// principal row lock, so no new issue/rotate can write more material until
// its transaction ends, and it calls this BEFORE its Postgres writes commit
// (material-first, §9.3). SecretStore.Delete is a no-op on a missing path, so
// a retry after a partial failure converges.
//
// perCall > 0 bounds each OpenBao call (TS-7 holds the principal lock across
// them, like TS-1/TS-2's inLockSecretWriteTimeout); 0 leaves them unbounded.
func EraseSecretMaterial(ctx context.Context, secrets port.SecretStore, paths []string, subtree string, perCall time.Duration) error {
	e := eraser{secrets: secrets, perCall: perCall}
	for _, path := range paths {
		if err := e.delete(ctx, path); err != nil {
			return err
		}
	}
	return e.deleteSubtree(ctx, subtree, 0)
}

type eraser struct {
	secrets port.SecretStore
	perCall time.Duration
}

func (e eraser) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.perCall <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.perCall)
}

func (e eraser) delete(ctx context.Context, path string) error {
	callCtx, cancel := e.bound(ctx)
	defer cancel()
	return e.secrets.Delete(callCtx, path)
}

// deleteSubtree deletes every leaf under prefix. SecretStore.List returns
// one level, with sub-folders suffixed "/".
func (e eraser) deleteSubtree(ctx context.Context, prefix string, depth int) error {
	if depth > maxSubtreeDepth {
		return nil
	}
	callCtx, cancel := e.bound(ctx)
	keys, err := e.secrets.List(callCtx, prefix)
	cancel()
	if err != nil {
		return err
	}
	for _, key := range keys {
		child := prefix + "/" + strings.TrimSuffix(key, "/")
		if strings.HasSuffix(key, "/") {
			if err := e.deleteSubtree(ctx, child, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := e.delete(ctx, child); err != nil {
			return err
		}
	}
	return nil
}
