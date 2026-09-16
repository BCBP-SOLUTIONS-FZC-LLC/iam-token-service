package port

import "context"

// SecretStore is the OpenBao KV v2 custody port (§6.3, §10.5). Read exists
// solely so TS-1's rotation_id-replay path (§9.2/§9.3 — a retried call
// under the same rotation_id returns the same version and secret without
// generating new material) can hand back the already-committed row's
// plaintext when the caller never saw the original response. No other path
// calls Read: TS-3 is metadata-only and every other response never carries
// a secret (TS-INV-2, §5.6) — "exactly once" means exactly one
// material-generation event per rotation_id, not that the bytes may cross
// the wire only once ever.
type SecretStore interface {
	// Write puts secret at the deterministic KV v2 path (§6.3,
	// domain.OpenBaoPathFor). Returns domain.ErrSecretStoreUnavailable on
	// failure (§5.4 TS-1 — nothing committed in Postgres when this fails).
	Write(ctx context.Context, path string, secret string) error

	// Read returns the plaintext at path — used only by TS-1's
	// rotation_id-replay path (§9.2). Returns domain.ErrSecretStoreUnavailable
	// on failure, including a missing path (the replay path only ever calls
	// this for a path an earlier Write already committed to Postgres, so a
	// 404 here is an unexpected divergence, not a normal no-op).
	Read(ctx context.Context, path string) (string, error)

	// Delete permanently removes all versions and metadata at path
	// (CUST-2 — a `revoked` row never has live OpenBao material; this is a
	// KV v2 metadata delete, not a soft/recoverable delete). A missing
	// path is a no-op, not an error, so revoke/offboarding/the §8.6
	// reconciler stay idempotent.
	Delete(ctx context.Context, path string) error

	// List returns the immediate child path segments under pathPrefix (KV
	// v2 LIST) — used by the §8.6 orphan-material reconciler to enumerate
	// a principal's committed and orphaned OpenBao versions.
	List(ctx context.Context, pathPrefix string) ([]string, error)
}
