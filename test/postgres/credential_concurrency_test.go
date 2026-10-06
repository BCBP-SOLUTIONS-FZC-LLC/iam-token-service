//go:build integration

package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// memSecretStore is a concurrency-safe in-memory port.SecretStore that keeps
// the LAST value written to each path — exactly the overwrite hazard the
// principal lock exists to prevent.
type memSecretStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newMemSecretStore() *memSecretStore { return &memSecretStore{data: map[string]string{}} }

func (m *memSecretStore) Write(_ context.Context, path, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[path] = secret
	return nil
}

func (m *memSecretStore) Read(_ context.Context, path string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[path]
	if !ok {
		return "", domain.NewError(domain.ErrSecretStoreUnavailable, "not found")
	}
	return v, nil
}

func (m *memSecretStore) Delete(_ context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, path)
	return nil
}

func (m *memSecretStore) List(context.Context, string) ([]string, error) { return nil, nil }

// TestCredentialService_ConcurrentIssueOrRotateIsSerialized: two TS-1 calls
// with DIFFERENT rotation_ids for the same principal, racing. Before the
// principal lock both computed the same next version and wrote the same
// OpenBao path, so the committed row could point at the losing caller's key.
// Now they serialize: each gets its own version, and every committed
// version's material is exactly the key returned to the caller that made it.
func TestCredentialService_ConcurrentIssueOrRotateIsSerialized(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	secrets := newMemSecretStore()

	// Slow key generation (it runs before the locked transaction) lines both
	// calls up so they reach the principal lock together.
	generate := func() (string, error) {
		time.Sleep(200 * time.Millisecond)
		return uuid.NewString(), nil
	}
	svc := service.NewCredentialService(
		pgadapter.NewPrincipalRepository(appPool), pgadapter.NewCredentialRepository(appPool),
		secrets, pgadapter.NewTxRunner(appPool, nil), nil, generate)

	tctx := withTenant(ctx, tenantID)
	_, err := svc.IssueOrRotate(tctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)

	results := make([]*service.IssueOrRotateResult, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.IssueOrRotate(tctx, tenantID, principalID,
				service.IssueOrRotateRequest{RotationID: uuid.New(), OverlapSeconds: 300}, domain.SystemPrincipalID)
		}()
	}
	wg.Wait()

	for i := range results {
		require.NoError(t, errs[i], "the second caller waits for the lock, then rotates on top of the first")
	}
	assert.NotEqual(t, results[0].Version, results[1].Version, "each caller gets its own version")
	assert.ElementsMatch(t, []int{2, 3}, []int{results[0].Version, results[1].Version})
	for _, res := range results {
		stored, err := secrets.Read(ctx, res.OpenBaoPath)
		require.NoError(t, err)
		assert.Equal(t, res.Secret, stored, "the committed version's material is the key returned for it")
	}

	var active int
	require.NoError(t, rawPool.QueryRow(ctx,
		`SELECT count(*) FROM service_account_credentials WHERE principal_id = $1 AND status = 'active'`, principalID).Scan(&active))
	assert.Equal(t, 1, active)
}

// TestCredentialService_IssueAfterRevokingActive_Postgres: the uq_sac_version
// regression against the real constraint.
func TestCredentialService_IssueAfterRevokingActive_Postgres(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	svc := service.NewCredentialService(
		pgadapter.NewPrincipalRepository(appPool), pgadapter.NewCredentialRepository(appPool),
		newMemSecretStore(), pgadapter.NewTxRunner(appPool, nil), nil, nil)
	tctx := withTenant(ctx, tenantID)

	for range 2 {
		_, err := svc.IssueOrRotate(tctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
		require.NoError(t, err)
	}
	_, err := svc.Revoke(tctx, tenantID, principalID, 2, domain.SystemPrincipalID)
	require.NoError(t, err)

	res, err := svc.IssueOrRotate(tctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.Equal(t, 3, res.Version)
}
