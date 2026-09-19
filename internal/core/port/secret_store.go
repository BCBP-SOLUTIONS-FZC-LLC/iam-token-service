package port

import "context"

// SecretStore is the OpenBao KV v2 custody port (§6.3, §10.5). Read has two
// callers: TS-1's rotation_id-replay path (§9.2/§9.3 — a retried call under
// the same rotation_id returns the same version and secret without
// generating new material) hands back the already-committed row's
// plaintext when the caller never saw the original response; and
// JWKSService (EXT-6, §2.5) derives a public JWK from the stored private
// key for every active/rotating credential on each JWKS fetch. TS-3 stays
// metadata-only and every other response never carries a secret (TS-INV-2,
// §5.6) — "exactly once" still means exactly one material-generation event
// per rotation_id, not that the bytes may cross the wire only once ever.
type SecretStore interface {
	// Write puts secret at the deterministic KV v2 path (§6.3,
	// domain.OpenBaoPathFor). Returns domain.ErrSecretStoreUnavailable on
	// failure (§5.4 TS-1 — nothing committed in Postgres when this fails).
	Write(ctx context.Context, path string, secret string) error

	// Read returns the plaintext at path — used by TS-1's
	// rotation_id-replay path (§9.2) and by JWKSService's per-fetch public-key
	// derivation. Returns domain.ErrSecretStoreUnavailable on failure,
	// including a missing path — for the replay path this is always an
	// unexpected divergence (it only ever reads a path an earlier Write
	// already committed to Postgres); JWKSService's caller does not treat a
	// missing path as fatal to the whole request, since it reads one
	// credential row at a time.
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
