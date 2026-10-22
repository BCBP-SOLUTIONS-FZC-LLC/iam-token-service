//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	eventbusadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/eventbus"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// TS-7 (RevokePrincipal) against a real Postgres: RLS-scoped repositories,
// the principal row lock, record_version triggers, and the real validating
// outbox publisher (so the emitted payloads are checked against the
// embedded schemas).

// kvStore is an in-memory KV v2 stand-in with real LIST semantics
// (immediate children, folders suffixed "/") and per-path delete failures.
type kvStore struct {
	mu        sync.Mutex
	data      map[string]string
	deleteErr map[string]error
}

func newKVStore() *kvStore { return &kvStore{data: map[string]string{}} }

func (s *kvStore) Write(_ context.Context, path, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[path] = secret
	return nil
}

func (s *kvStore) Read(_ context.Context, path string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[path]
	if !ok {
		return "", domain.NewError(domain.ErrSecretStoreUnavailable, "not found")
	}
	return v, nil
}

func (s *kvStore) Delete(_ context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deleteErr[path]; err != nil {
		return err
	}
	delete(s.data, path)
	return nil
}

func (s *kvStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for k := range s.data {
		rest, ok := strings.CutPrefix(k, prefix+"/")
		if !ok {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i+1]
		}
		if !seen[rest] {
			seen[rest] = true
			out = append(out, rest)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *kvStore) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// failingPublisher passes events through to the real outbox publisher but
// fails the one whose type is failType — a fault after the credential rows
// were written, to prove the whole revoke rolls back.
type failingPublisher struct {
	inner    port.EventPublisher
	failType string
}

func (p *failingPublisher) Enqueue(ctx context.Context, e *domain.Event) error {
	if e.Type == p.failType {
		return errors.New("outbox write failed")
	}
	return p.inner.Enqueue(ctx, e)
}

func outboxPublisher(t *testing.T) port.EventPublisher {
	t.Helper()
	codec, err := eventbusadapter.NewValidatingCodec(eventbusadapter.NoopCodec{})
	require.NoError(t, err)
	return eventbusadapter.New("iam-token-service", codec)
}

func newRevokeService(pool *pgcommon.Pool, secrets port.SecretStore, events port.EventPublisher) *service.CredentialService {
	return service.NewCredentialService(
		pgadapter.NewPrincipalRepository(pool), pgadapter.NewCredentialRepository(pool),
		secrets, pgadapter.NewTxRunner(pool, events), nil,
		func() (string, error) { return uuid.NewString(), nil })
}

type ts7Row struct {
	status        string
	recordVersion int
}

func principalRow(t *testing.T, rawPool *dbseed.Pool, principalID uuid.UUID) ts7Row {
	t.Helper()
	var r ts7Row
	require.NoError(t, rawPool.QueryRow(context.Background(),
		`SELECT status::text, record_version FROM service_account_principals WHERE id = $1`, principalID).
		Scan(&r.status, &r.recordVersion))
	return r
}

// credentialRows returns version → "status/next_rotation_at set".
func credentialRows(t *testing.T, rawPool *dbseed.Pool, principalID uuid.UUID) map[int]string {
	t.Helper()
	rows, err := rawPool.Query(context.Background(),
		`SELECT version, status::text, next_rotation_at IS NOT NULL FROM service_account_credentials WHERE principal_id = $1`, principalID)
	require.NoError(t, err)
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var (
			v         int
			status    string
			scheduled bool
		)
		require.NoError(t, rows.Scan(&v, &status, &scheduled))
		if scheduled {
			status += "/scheduled"
		}
		out[v] = status
	}
	require.NoError(t, rows.Err())
	return out
}

func outboxTypes(t *testing.T, rawPool *dbseed.Pool, tenantID uuid.UUID) []string {
	t.Helper()
	rows, err := rawPool.Query(context.Background(),
		`SELECT event_type FROM outbox_events WHERE tenant_id = $1 ORDER BY created_at, event_type`, tenantID.String())
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

// seedIssued issues v1 then rotates to v2 through the real service (v1
// rotating for 300s, v2 active and scheduled), plus orphaned material at v9.
func seedIssued(t *testing.T, svc *service.CredentialService, secrets *kvStore, tenantID, principalID uuid.UUID) {
	t.Helper()
	ctx := withTenant(context.Background(), tenantID)
	for range 2 {
		_, err := svc.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, domain.SystemPrincipalID)
		require.NoError(t, err)
	}
	require.NoError(t, secrets.Write(ctx, domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 9), "orphan"))
}

func TestRevokePrincipal_Postgres_RevokesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	tenantID := uuid.New()
	principalID := seedPrincipal(t, context.Background(), rawPool, tenantID)
	secrets := newKVStore()
	svc := newRevokeService(appPool, secrets, outboxPublisher(t))
	seedIssued(t, svc, secrets, tenantID, principalID)
	before := len(outboxTypes(t, rawPool, tenantID))
	ctx := withTenant(context.Background(), tenantID)

	res, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, principalID, res.PrincipalID)
	assert.Equal(t, domain.PrincipalStatusRevoked, res.Status)
	assert.Equal(t, []int{1, 2}, res.Versions)
	assert.Equal(t, 2, res.NewlyRevoked)

	assert.Equal(t, ts7Row{status: "revoked", recordVersion: 2}, principalRow(t, rawPool, principalID))
	assert.Equal(t, res.RecordVersion, 2)
	assert.Equal(t, map[int]string{1: "revoked", 2: "revoked"}, credentialRows(t, rawPool, principalID),
		"every version revoked, none still scheduled for rotation")
	assert.Empty(t, secrets.paths(), "row material and the orphan under the client prefix are gone")

	emitted := outboxTypes(t, rawPool, tenantID)[before:]
	assert.ElementsMatch(t, []string{
		domain.EventServiceAccountCredentialRevoked, domain.EventServiceAccountCredentialRevoked,
		domain.EventServiceAccountRevoked,
	}, emitted)

	again, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Zero(t, again.NewlyRevoked)
	again.NewlyRevoked = res.NewlyRevoked
	assert.Equal(t, res.RevokedAt.UTC().Truncate(time.Second), again.RevokedAt.UTC().Truncate(time.Second))
	again.RevokedAt = res.RevokedAt
	assert.Equal(t, res, again, "idempotent: the same body")
	assert.Len(t, outboxTypes(t, rawPool, tenantID), before+3, "no new events")

	// Terminal: TS-1 refuses, TS-4 cannot reactivate it.
	_, err = svc.IssueOrRotate(ctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalRevoked, de.Code)
}

func TestRevokePrincipal_Postgres_NotFoundAndRLS(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	other := uuid.New()
	seedPrincipal(t, context.Background(), rawPool, other)
	svc := newRevokeService(appPool, newKVStore(), outboxPublisher(t))

	tenantID := uuid.New()
	_, err := svc.RevokePrincipal(withTenant(context.Background(), tenantID), tenantID, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalNotFound, de.Code)

	// Under tenant A's GUC, tenant B's principal is invisible even when named.
	_, err = svc.RevokePrincipal(withTenant(context.Background(), tenantID), other, domain.SystemPrincipalID)
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalNotFound, de.Code)
	assert.Equal(t, "active", principalRowByTenant(t, rawPool, other))
}

func principalRowByTenant(t *testing.T, rawPool *dbseed.Pool, tenantID uuid.UUID) string {
	t.Helper()
	var s string
	require.NoError(t, rawPool.QueryRow(context.Background(),
		`SELECT status::text FROM service_account_principals WHERE tenant_id = $1`, tenantID).Scan(&s))
	return s
}

// An in-flight rotation holds the principal lock past PG_LOCK_TIMEOUT: 409
// rotation_in_flight with §17's active_rotation_id; nothing changes.
func TestRevokePrincipal_Postgres_RotationInFlight(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	appPool := shortLockTimeoutAppPool(t, rawPool)
	tenantID := uuid.New()
	principalID := seedPrincipal(t, context.Background(), rawPool, tenantID)
	secrets := newKVStore()
	svc := newRevokeService(appPool, secrets, nil)
	seedIssued(t, svc, secrets, tenantID, principalID)
	before := credentialRows(t, rawPool, principalID)
	material := secrets.paths()

	release := holdPrincipalLock(t, rawPool, tenantID)
	_, err := svc.RevokePrincipal(withTenant(context.Background(), tenantID), tenantID, domain.SystemPrincipalID)
	release()

	requireRotationInFlight(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.NotNil(t, de.Details["active_rotation_id"])
	assert.Equal(t, ts7Row{status: "active", recordVersion: 1}, principalRow(t, rawPool, principalID))
	assert.Equal(t, before, credentialRows(t, rawPool, principalID))
	assert.Equal(t, material, secrets.paths(), "no OpenBao call before the lock")
}

// An OpenBao delete failure is a 502 with nothing committed; the retry
// converges.
func TestRevokePrincipal_Postgres_OpenBaoFailureThenRetry(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	tenantID := uuid.New()
	principalID := seedPrincipal(t, context.Background(), rawPool, tenantID)
	secrets := newKVStore()
	svc := newRevokeService(appPool, secrets, outboxPublisher(t))
	seedIssued(t, svc, secrets, tenantID, principalID)
	before := credentialRows(t, rawPool, principalID)
	events := len(outboxTypes(t, rawPool, tenantID))
	ctx := withTenant(context.Background(), tenantID)

	v2 := domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 2)
	secrets.deleteErr = map[string]error{v2: domain.NewError(domain.ErrSecretStoreUnavailable, "openbao down")}
	_, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrSecretStoreUnavailable, de.Code)
	assert.Equal(t, ts7Row{status: "active", recordVersion: 1}, principalRow(t, rawPool, principalID))
	assert.Equal(t, before, credentialRows(t, rawPool, principalID))
	assert.Len(t, outboxTypes(t, rawPool, tenantID), events, "no event for a failed revoke")

	secrets.deleteErr = nil
	res, err := svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, 2, res.NewlyRevoked)
	assert.Equal(t, "revoked", principalRow(t, rawPool, principalID).status)
	assert.Empty(t, secrets.paths())
}

// A failure after the credential rows were written (the principal-level
// event's outbox write) rolls the whole revoke back: no credential is left
// `revoked` under a still-active principal.
func TestRevokePrincipal_Postgres_LateFailureRollsEverythingBack(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	tenantID := uuid.New()
	principalID := seedPrincipal(t, context.Background(), rawPool, tenantID)
	secrets := newKVStore()
	seedIssued(t, newRevokeService(appPool, secrets, nil), secrets, tenantID, principalID)
	before := credentialRows(t, rawPool, principalID)
	ctx := withTenant(context.Background(), tenantID)

	failing := newRevokeService(appPool, secrets,
		&failingPublisher{inner: outboxPublisher(t), failType: domain.EventServiceAccountRevoked})
	_, err := failing.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.Error(t, err)
	assert.Equal(t, ts7Row{status: "active", recordVersion: 1}, principalRow(t, rawPool, principalID))
	assert.Equal(t, before, credentialRows(t, rawPool, principalID), "credential updates rolled back")
	assert.Empty(t, outboxTypes(t, rawPool, tenantID), "per-version events rolled back with them")

	res, err := newRevokeService(appPool, secrets, outboxPublisher(t)).RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, 2, res.NewlyRevoked, "the retry converges")
}

// Two concurrent TS-7 calls serialize on the principal lock: both succeed,
// one revokes, the other reports the same end state; one set of events.
func TestRevokePrincipal_Postgres_ConcurrentCallsRevokeOnce(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	tenantID := uuid.New()
	principalID := seedPrincipal(t, context.Background(), rawPool, tenantID)
	secrets := newKVStore()
	svc := newRevokeService(appPool, secrets, outboxPublisher(t))
	seedIssued(t, svc, secrets, tenantID, principalID)
	before := len(outboxTypes(t, rawPool, tenantID))
	ctx := withTenant(context.Background(), tenantID)

	var wg sync.WaitGroup
	results := make([]*service.RevokePrincipalResult, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.RevokePrincipal(ctx, tenantID, domain.SystemPrincipalID)
		}()
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, 2, results[0].NewlyRevoked+results[1].NewlyRevoked)
	assert.Equal(t, results[0].RecordVersion, results[1].RecordVersion)
	assert.Len(t, outboxTypes(t, rawPool, tenantID), before+3)
}
