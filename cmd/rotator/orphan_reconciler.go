package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// materialReconcileResult summarizes one orphan-material reconciler run
// (§8.6). MissingMaterial is page-worthy (iam_token_service_material_reconcile_total{result="missing_material"} > 0, §11.5).
type materialReconcileResult struct {
	OrphanDeleted   int
	MissingMaterial int
	OK              int
	Failed          int
}

// runOrphanMaterialReconciler implements §8.6: for every principal
// (enumerated cross-tenant via the BYPASSRLS reconciler role), list its
// OpenBao material and reconcile against its committed credential
// versions. Deletes only OpenBao material — no Postgres write, so no RLS
// GUC binding is needed here (contrast sweep.go).
func runOrphanMaterialReconciler(ctx context.Context, reconciler port.ReconcilerRepository, secrets port.SecretStore, log port.Logger) materialReconcileResult {
	var result materialReconcileResult
	states, err := reconciler.ListPrincipalMaterialStates(ctx)
	if err != nil {
		logError(ctx, log, "orphan reconciler: list principal material states failed", nil, err)
		return result
	}
	for _, st := range states {
		reconcileOnePrincipal(ctx, secrets, log, st, &result)
	}
	return result
}

// reconcileOnePrincipal lists st's OpenBao material and, per version
// found: leaves it alone if committed; deletes it if uncommitted AND below
// MaxVersion (a genuine orphan, §9.3 CUST-3); leaves it alone if
// uncommitted but >= MaxVersion (may belong to a concurrently-committing
// issue/rotate — the reconciler never races a live TS-1, §8.6). Any
// committed version with no matching OpenBao entry is the inverse
// divergence — surfaced, never auto-repaired (§8.6: "resolved by rotation,
// never by fabricating material").
func reconcileOnePrincipal(ctx context.Context, secrets port.SecretStore, log port.Logger, st port.PrincipalMaterialState, result *materialReconcileResult) {
	prefix := fmt.Sprintf("iam/serviceaccount/%s/%s", st.TenantID, st.KeycloakClientID)
	keys, err := secrets.List(ctx, prefix)
	if err != nil {
		result.Failed++
		logError(ctx, log, "orphan reconciler: list openbao material failed", map[string]interface{}{
			"tenant_id": st.TenantID, "principal_id": st.PrincipalID,
		}, err)
		return
	}

	committed := make(map[int]bool, len(st.CommittedVersions))
	for _, v := range st.CommittedVersions {
		committed[v] = true
	}

	found := make(map[int]bool, len(keys))
	for _, key := range keys {
		version, ok := parseVersionKey(key)
		if !ok {
			continue
		}
		found[version] = true

		if committed[version] {
			result.OK++
			continue
		}
		if version >= st.MaxVersion {
			continue
		}
		path := domain.OpenBaoPathFor(st.TenantID, st.KeycloakClientID, version)
		if err := secrets.Delete(ctx, path); err != nil {
			result.Failed++
			logError(ctx, log, "orphan reconciler: delete orphaned material failed", map[string]interface{}{
				"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": version,
			}, err)
			continue
		}
		result.OrphanDeleted++
	}

	for version := range committed {
		if found[version] {
			continue
		}
		result.MissingMaterial++
		if log != nil {
			log.Error("orphan reconciler: committed credential has no OpenBao material — page-worthy divergence, resolve by rotation (§8.6)", fieldsWithTrace(ctx, map[string]interface{}{
				"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": version,
			}))
		}
	}
}

// parseVersionKey parses a KV v2 List() leaf key (e.g. "v3", possibly with
// a trailing "/") into its integer version.
func parseVersionKey(key string) (int, bool) {
	key = strings.TrimSuffix(key, "/")
	if !strings.HasPrefix(key, "v") {
		return 0, false
	}
	n, err := strconv.Atoi(key[1:])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}
