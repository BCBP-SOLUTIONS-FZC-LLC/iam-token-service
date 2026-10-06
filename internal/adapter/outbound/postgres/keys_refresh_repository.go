package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// KeysRefreshMark is the outcome of KeysRefreshRepository.MarkPending.
type KeysRefreshMark struct {
	// RequestedAt is the marker's value after the upsert — the token a later
	// Clear passes as upTo, so it never clears a request newer than this one.
	RequestedAt time.Time
	// Fresh is true when no marker existed before this upsert, i.e. no other
	// refresh was owed for the tenant. Only then may a caller that ends up
	// committing nothing clear the marker again without calling RP-17.
	Fresh bool
}

// PendingKeysRefresh is one owed RP-17 refresh, as the retry pass sees it.
type PendingKeysRefresh struct {
	TenantID    uuid.UUID
	RequestedAt time.Time
	// HasPrincipal reports whether the tenant still has a principal. A tenant
	// offboarded since the marker was written has no Keycloak client left to
	// protect, so a failing RP-17 for it must not page on every run forever.
	HasPrincipal bool
}

// KeysRefreshRepository reads and writes keys_refresh_pending (migration
// 000005), the durable "RP-17 key-cache refresh owed for this tenant" marker
// written by cmd/rotator and cmd/scheduler only. MarkPending, MarkIntent and
// Clear run on the app pool (each joins the caller's transaction when ctx
// carries one, so the sweep writes the marker atomically with its revoke);
// ListPending and PendingStats use the BYPASSRLS reconciler pool, since
// ListPending also reads service_account_principals across tenants and
// PendingStats is cmd/server's cross-tenant gauge read. Like
// RLSViolationRepository it implements no core port.
type KeysRefreshRepository struct {
	app        *pgcommon.Pool
	reconciler *pgcommon.Pool
}

// NewKeysRefreshRepository constructs a KeysRefreshRepository over the app
// pool (writes) and the reconciler pool (cross-tenant listing).
func NewKeysRefreshRepository(app, reconciler *pgcommon.Pool) *KeysRefreshRepository {
	return &KeysRefreshRepository{app: app, reconciler: reconciler}
}

// MarkPending upserts tenantID's COMMITTED marker (intent_until NULL): the
// revoke or rotation it covers has committed (or, for the sweep, commits in
// the same transaction), so the retry pass may take it at once. It also
// turns an existing intent marker into a committed one. requested_at only
// ever moves forward and is taken from clock_timestamp() at the moment the
// row lock is held, so markers written by concurrent transactions are
// ordered the same way their row locks were granted — a Clear(upTo) of an
// older value can never delete a newer request. (xmax = 0) identifies a
// freshly inserted row as opposed to one ON CONFLICT updated.
func (r *KeysRefreshRepository) MarkPending(ctx context.Context, tenantID uuid.UUID) (KeysRefreshMark, error) {
	var mark KeysRefreshMark
	err := withPool(ctx, r.app, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO keys_refresh_pending (tenant_id, requested_at, intent_until) VALUES ($1, clock_timestamp(), NULL)
			ON CONFLICT (tenant_id) DO UPDATE
			   SET requested_at = GREATEST(keys_refresh_pending.requested_at, clock_timestamp()),
			       intent_until = NULL
			RETURNING requested_at, (xmax = 0)`, tenantID).Scan(&mark.RequestedAt, &mark.Fresh)
	})
	return mark, err
}

// MarkIntent upserts tenantID's INTENT marker: cmd/scheduler writes it
// before an IssueOrRotate that may or may not commit, and the retry pass
// ignores it until intent_until (now + ttl) has passed — by then its writer
// has converted it (MarkPending), cleared it, or died, and an expired intent
// is retried like a committed marker. It never downgrades a committed marker
// (intent_until NULL stays NULL) or an expired intent (treated as committed:
// its writer is gone, so extending it would only delay a refresh owed);
// otherwise it extends the intent to the later of the two deadlines.
func (r *KeysRefreshRepository) MarkIntent(ctx context.Context, tenantID uuid.UUID, ttl time.Duration) (KeysRefreshMark, error) {
	var mark KeysRefreshMark
	err := withPool(ctx, r.app, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO keys_refresh_pending (tenant_id, requested_at, intent_until)
			VALUES ($1, clock_timestamp(), clock_timestamp() + make_interval(secs => $2))
			ON CONFLICT (tenant_id) DO UPDATE
			   SET requested_at = GREATEST(keys_refresh_pending.requested_at, clock_timestamp()),
			       intent_until = CASE
			         WHEN keys_refresh_pending.intent_until IS NULL
			           OR keys_refresh_pending.intent_until < clock_timestamp() THEN NULL
			         ELSE GREATEST(keys_refresh_pending.intent_until, EXCLUDED.intent_until)
			       END
			RETURNING requested_at, (xmax = 0)`, tenantID, ttl.Seconds()).Scan(&mark.RequestedAt, &mark.Fresh)
	})
	return mark, err
}

// Clear deletes tenantID's marker if its requested_at is not newer than
// upTo — the value observed before the RP-17 call that satisfied it. A
// request written after that (by a concurrent run) survives for its own
// refresh. Clearing an absent marker is a no-op.
func (r *KeysRefreshRepository) Clear(ctx context.Context, tenantID uuid.UUID, upTo time.Time) error {
	return withPool(ctx, r.app, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM keys_refresh_pending WHERE tenant_id = $1 AND requested_at <= $2`, tenantID, upTo)
		return err
	})
}

// ListPending returns up to limit owed refreshes, oldest first: every
// committed marker, and every intent marker whose intent_until has passed.
// A live intent is left to its writer — a cmd/scheduler run between writing
// it and committing its rotation, which calls RP-17 itself once it commits;
// retrying (and clearing) it now would refresh before the commit and then
// forget the refresh if that run died before its own call.
func (r *KeysRefreshRepository) ListPending(ctx context.Context, limit int) ([]PendingKeysRefresh, error) {
	var out []PendingKeysRefresh
	err := withPool(ctx, r.reconciler, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT k.tenant_id, k.requested_at,
			       EXISTS (SELECT 1 FROM service_account_principals p
			                WHERE p.tenant_id = k.tenant_id AND p.deleted_at IS NULL)
			FROM keys_refresh_pending k
			WHERE k.intent_until IS NULL OR k.intent_until < now()
			ORDER BY k.requested_at, k.tenant_id
			LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p PendingKeysRefresh
			if err := rows.Scan(&p.TenantID, &p.RequestedAt, &p.HasPrincipal); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PendingStats reports every marker currently in keys_refresh_pending —
// committed and intent alike — and the age of the oldest (now() minus the
// minimum requested_at; 0 when the table is empty). Read over the
// reconciler pool; cmd/server's exporter loop turns it into
// iam_token_service_keys_refresh_pending / _oldest_age_seconds, so a refresh
// that keeps failing is visible between CronJob runs. The age is clamped
// at 0: a marker written by a transaction that started after this one can
// carry a requested_at later than this transaction's now().
func (r *KeysRefreshRepository) PendingStats(ctx context.Context) (int, time.Duration, error) {
	var (
		count      int
		ageSeconds float64
	)
	err := withPool(ctx, r.reconciler, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(requested_at)), 0)::float8
			FROM keys_refresh_pending`).Scan(&count, &ageSeconds)
	})
	if err != nil {
		return 0, 0, err
	}
	if ageSeconds < 0 {
		ageSeconds = 0
	}
	return count, time.Duration(ageSeconds * float64(time.Second)), nil
}
