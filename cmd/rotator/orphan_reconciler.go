package main

import (
	"context"
	"errors"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// materialReconcileResult summarizes one orphan-material reconciler run
// (§8.6). MissingMaterial is page-worthy (iam_token_service_material_reconcile_total{result="missing_material"} > 0, §11.5).
// Skipped and Deferred are logged only (no metric label): neither is a
// failure, and neither fails the Job.
type materialReconcileResult struct {
	OrphanDeleted   int
	MissingMaterial int
	OK              int
	Failed          int
	Skipped         int // orphan candidates left alone this run: a TS-1 held the principal lock, or the principal is gone
	Deferred        int // principals not started: out of run budget, or the run was signalled
}

// principalLocker is the one PrincipalRepository method the reconciler
// needs (*pgadapter.PrincipalRepository satisfies it).
type principalLocker interface {
	LockForUpdate(ctx context.Context, tenantID, principalID uuid.UUID) (*domain.ServiceAccountPrincipal, error)
}

// inLockSecretTimeout bounds every OpenBao call the reconciler makes while
// it holds a principal row lock. It mirrors the service's
// inLockSecretWriteTimeout (internal/core/service/credential_service.go,
// 5s): every TS-1/TS-2/TS-4 call for the principal waits on that lock only
// PG_LOCK_TIMEOUT before failing 409 rotation_in_flight, so an unbounded
// OpenBao call (HTTPTimeout per attempt plus a 403 re-login retry) would
// turn into an operator-visible outage. cmd/rotator may not import
// service, hence the local copy.
const inLockSecretTimeout = 5 * time.Second

// orphanLock is what the reconciler needs to delete orphan material safely:
// the RLS-scoped app pool's TxRunner, the principal row lock TS-1 holds
// across its OpenBao write and its commit, and a re-read of the credential
// row under that lock.
type orphanLock struct {
	tx          port.TxRunner
	principals  principalLocker
	credentials port.CredentialRepository
}

// runOrphanMaterialReconciler implements §8.6: for every principal
// (enumerated cross-tenant via the BYPASSRLS reconciler role), list its
// OpenBao material and reconcile it against its credential rows. A failed
// enumeration is a run failure (the job exits non-zero), never a silent
// empty result.
//
// The phase is budget-guarded like the sweep: a principal starts only with
// rowReserve left (and not after SIGTERM); the rest are Deferred, not
// Failed, so a long sweep no longer turns the reconciler into a page.
// Enumeration starts at an offset derived from seed (main passes the run's
// start time) and wraps around, so when the budget runs out it is not
// always the same high-id principals that are never reached.
func runOrphanMaterialReconciler(ctx context.Context, reconciler port.ReconcilerRepository, secrets port.SecretStore, lock orphanLock, seed int64, log port.Logger) materialReconcileResult {
	var result materialReconcileResult
	states, err := reconciler.ListPrincipalMaterialStates(ctx)
	if err != nil {
		result.Failed++
		logError(ctx, log, "orphan reconciler: list principal material states failed", nil, err)
		return result
	}
	n := len(states)
	if n == 0 {
		return result
	}
	offset := int(uint64(seed) % uint64(n)) // #nosec G115 -- a rotation offset, not a security value; the modulo keeps it in [0,n)
	for i := 0; i < n; i++ {
		if stopStarting(ctx, rowReserve) {
			result.Deferred = n - i
			if log != nil {
				log.Warn("orphan reconciler: run budget nearly spent or run signalled — deferring the remaining principals to the next run", fieldsWithTrace(ctx, map[string]interface{}{"deferred": result.Deferred}))
			}
			break
		}
		wctx, cancel := workCtx(ctx)
		reconcileOnePrincipal(wctx, reconciler, secrets, lock, log, states[(offset+i)%n], &result)
		cancel()
	}
	return result
}

// reconcileOnePrincipal lists every OpenBao prefix st's material can live
// under — the current client id's, plus the parent of every stored
// openbao_path (a re-mint that changed keycloak_client_id leaves older
// versions under the old prefix) — and, per version found:
//   - a LIVE row (active/rotating) at that path: fine;
//   - a REVOKED row: stale material a crashed revoke left behind — deleted
//     (CUST-2: a revoked credential's material must not survive);
//   - no row in the snapshot: an orphan candidate (§9.3 CUST-3), deleted
//     only under the principal's row lock after re-checking that no row
//     claims the version (deleteOrphanUnderLock). This replaces the old
//     "version >= MaxVersion may be in flight" heuristic, which also meant a
//     principal with no rows at all (MaxVersion 0) never had anything
//     reclaimed.
//
// A live row whose path holds no material is the inverse divergence —
// surfaced (page-worthy), never auto-repaired (§8.6: "resolved by rotation,
// never by fabricating material") — but only after re-checking both the row
// and OpenBao under the principal lock (confirmMissingUnderLock): a revoke
// that committed after the snapshot, or is between its material delete and
// its commit right now, is expected to have deleted it.
// Revoked rows are never reported missing: their material is deleted by
// design.
func reconcileOnePrincipal(ctx context.Context, reconciler port.ReconcilerRepository, secrets port.SecretStore, lock orphanLock, log port.Logger, st port.PrincipalMaterialState, result *materialReconcileResult) {
	byPath := make(map[string]port.CredentialMaterial, len(st.Credentials))
	prefixes := []string{domain.OpenBaoPathPrefixFor(st.TenantID, st.KeycloakClientID)}
	seenPrefix := map[string]bool{prefixes[0]: true}
	for _, c := range st.Credentials {
		byPath[c.OpenBaoPath] = c
		if dir := path.Dir(c.OpenBaoPath); c.OpenBaoPath != "" && !seenPrefix[dir] {
			seenPrefix[dir] = true
			prefixes = append(prefixes, dir)
		}
	}
	rowVersions := make(map[int]bool, len(st.Credentials))
	for _, c := range st.Credentials {
		rowVersions[c.Version] = true
	}

	found := map[string]bool{}
	for _, prefix := range prefixes {
		keys, err := secrets.List(ctx, prefix)
		if err != nil {
			result.Failed++
			logError(ctx, log, "orphan reconciler: list openbao material failed", map[string]interface{}{
				"tenant_id": st.TenantID, "principal_id": st.PrincipalID,
			}, err)
			return
		}
		for _, key := range keys {
			version, ok := parseVersionKey(key)
			if !ok {
				continue
			}
			full := prefix + "/v" + strconv.Itoa(version)
			found[full] = true

			if row, known := byPath[full]; known {
				if row.Live {
					result.OK++
					continue
				}
				// Revoked row whose material survived (a revoke that crashed
				// between its delete and its commit, then committed on retry).
				// `revoked` is terminal, so no lock is needed.
				deleteMaterial(ctx, secrets, log, st, version, full, result)
				continue
			}
			if rowVersions[version] {
				// Same version recorded under another prefix — left for that
				// prefix's own check.
				continue
			}
			deleteOrphanUnderLock(ctx, secrets, lock, log, st, version, full, result)
		}
	}

	for _, c := range st.Credentials {
		if !c.Live || found[c.OpenBaoPath] {
			continue
		}
		// Cheap unlocked pre-filter: a row already revoked since the snapshot
		// needs no lock.
		live, err := reconciler.IsCredentialLive(ctx, st.PrincipalID, c.Version)
		if err != nil {
			result.Failed++
			logError(ctx, log, "orphan reconciler: re-check credential status failed", map[string]interface{}{
				"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": c.Version,
			}, err)
			continue
		}
		if !live {
			continue // revoked since the snapshot — its material is gone by design
		}
		confirmMissingUnderLock(ctx, secrets, lock, log, st, c, result)
	}
}

// confirmMissingUnderLock decides whether a live row whose material the
// unlocked List did not find is a genuine missing_material divergence. TS-2
// deletes material BEFORE it commits the revoke (material-first), holding
// the principal row lock across both — so between the two an unlocked
// IsCredentialLive still says live while OpenBao already has nothing, which
// used to page (and fail the Job) on every revoke the reconciler happened
// to overlap. Under that same lock, in an RLS-scoped app-pool transaction,
// a revoke is either fully committed (row not live → OK) or not started
// (material present → OK); only a live row with no material at that point
// is reported. Existence is re-checked with List on the row's own prefix,
// not Read: Read cannot tell a missing path from an OpenBao outage (both
// are secret_store_unavailable) and would pull private-key plaintext into
// this process for nothing. A busy lock (55P03 → ErrRotationInFlight) or a
// principal gone since the snapshot is Skipped, like deleteOrphanUnderLock.
func confirmMissingUnderLock(ctx context.Context, secrets port.SecretStore, lock orphanLock, log port.Logger, st port.PrincipalMaterialState, c port.CredentialMaterial, result *materialReconcileResult) {
	var missing bool
	err := lock.tx.RunInTx(withTenantGUC(ctx, st.TenantID), func(ctx context.Context) error {
		missing = false
		if _, err := lock.principals.LockForUpdate(ctx, st.TenantID, st.PrincipalID); err != nil {
			return err
		}
		cred, err := lock.credentials.FindByVersion(ctx, st.TenantID, st.PrincipalID, c.Version)
		if err != nil {
			return err
		}
		if cred == nil || cred.Status == domain.CredentialStatusRevoked {
			return nil // revoked (or gone) since the snapshot — material deleted by design
		}
		materialPath := cred.OpenBaoPath
		if materialPath == "" {
			materialPath = c.OpenBaoPath
		}
		// Bounded for the same reason as the orphan delete: the principal
		// lock is held.
		lctx, cancel := context.WithTimeout(ctx, inLockSecretTimeout)
		defer cancel()
		keys, err := secrets.List(lctx, path.Dir(materialPath))
		if err != nil {
			return err
		}
		want := path.Base(materialPath)
		for _, k := range keys {
			if strings.TrimSuffix(k, "/") == want {
				return nil
			}
		}
		missing = true
		return nil
	})
	var de *domain.Error
	switch {
	case err == nil && missing:
		result.MissingMaterial++
		if log != nil {
			log.Error("orphan reconciler: live credential has no OpenBao material — page-worthy divergence, resolve by rotation (§8.6)", fieldsWithTrace(ctx, map[string]interface{}{
				"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": c.Version,
			}))
		}
	case err == nil:
		result.OK++
	case errors.As(err, &de) && (de.Code == domain.ErrRotationInFlight || de.Code == domain.ErrPrincipalNotFound):
		result.Skipped++
		if log != nil {
			log.Info("orphan reconciler: principal busy or gone — missing-material check left for the next run", fieldsWithTrace(ctx, map[string]interface{}{
				"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": c.Version, "reason": string(de.Code),
			}))
		}
	default:
		result.Failed++
		logError(ctx, log, "orphan reconciler: missing-material re-check under the principal lock failed", map[string]interface{}{
			"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": c.Version,
		}, err)
	}
}

// deleteOrphanUnderLock deletes material no credential row claimed in the
// snapshot, but only inside an RLS-scoped app-pool transaction that holds
// the principal's row lock (PrincipalRepository.LockForUpdate) and has
// re-checked that still no row exists for the version. TS-1 holds that
// same lock across its OpenBao write and its commit, so once the lock is
// ours no issue/rotate can be between the two: material without a row is a
// genuine orphan (a TS-1 that crashed before committing). A lock wait that
// times out (55P03 → domain.ErrRotationInFlight) means a TS-1 is in flight
// right now, and a principal gone since the snapshot has nothing left to
// reconcile — both are Skipped for this run, not Failed.
func deleteOrphanUnderLock(ctx context.Context, secrets port.SecretStore, lock orphanLock, log port.Logger, st port.PrincipalMaterialState, version int, materialPath string, result *materialReconcileResult) {
	var deleted bool
	err := lock.tx.RunInTx(withTenantGUC(ctx, st.TenantID), func(ctx context.Context) error {
		deleted = false
		if _, err := lock.principals.LockForUpdate(ctx, st.TenantID, st.PrincipalID); err != nil {
			return err
		}
		cred, err := lock.credentials.FindByVersion(ctx, st.TenantID, st.PrincipalID, version)
		if err != nil {
			return err
		}
		if cred != nil {
			return nil // committed since the snapshot — not an orphan
		}
		// Bounded: TS-1 and TS-2 wait on this lock only PG_LOCK_TIMEOUT, so
		// an OpenBao call hanging here would fail every one of them.
		dctx, cancel := context.WithTimeout(ctx, inLockSecretTimeout)
		defer cancel()
		if err := secrets.Delete(dctx, materialPath); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	var de *domain.Error
	switch {
	case err == nil && deleted:
		result.OrphanDeleted++
	case err == nil:
		result.Skipped++
	case errors.As(err, &de) && (de.Code == domain.ErrRotationInFlight || de.Code == domain.ErrPrincipalNotFound):
		result.Skipped++
		if log != nil {
			log.Info("orphan reconciler: principal busy or gone — orphan candidate left for the next run", fieldsWithTrace(ctx, map[string]interface{}{
				"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": version, "reason": string(de.Code),
			}))
		}
	default:
		result.Failed++
		logError(ctx, log, "orphan reconciler: delete orphaned material failed", map[string]interface{}{
			"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": version,
		}, err)
	}
}

func deleteMaterial(ctx context.Context, secrets port.SecretStore, log port.Logger, st port.PrincipalMaterialState, version int, materialPath string, result *materialReconcileResult) {
	if err := secrets.Delete(ctx, materialPath); err != nil {
		result.Failed++
		logError(ctx, log, "orphan reconciler: delete orphaned material failed", map[string]interface{}{
			"tenant_id": st.TenantID, "principal_id": st.PrincipalID, "version": version,
		}, err)
		return
	}
	result.OrphanDeleted++
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
