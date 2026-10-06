//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/test/dbseed"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// shortLockTimeoutAppPool is a serviceaccount_app pool whose transactions
// run under a 200ms lock_timeout (pgcommon's per-transaction SET LOCAL, the
// PG_LOCK_TIMEOUT path production uses), so a blocked row lock fails fast
// with SQLSTATE 55P03.
func shortLockTimeoutAppPool(t *testing.T, rawPool *dbseed.Pool) *pgcommon.Pool {
	t.Helper()
	dsn := strings.Replace(rawPool.Config().ConnString(), "postgres:testpassword@", "serviceaccount_app:"+appRolePassword+"@", 1)
	pool, err := pgcommon.NewPool(context.Background(), pgcommon.Config{
		DSN: dsn, GUCProvider: pgcommon.GUCSetFromContext, LockTimeout: 200 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// holdPrincipalLock locks tenantID's principal rows FOR UPDATE in a
// superuser transaction (standing in for a TS-1 mid-rotation) and keeps the
// lock until the returned release func is called.
func holdPrincipalLock(t *testing.T, rawPool *dbseed.Pool, tenantID uuid.UUID) (release func()) {
	t.Helper()
	locked := make(chan struct{})
	done := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- rawPool.WithTx(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM service_account_principals WHERE tenant_id = $1 FOR UPDATE`, tenantID); err != nil {
				close(locked)
				return err
			}
			close(locked)
			<-done
			return nil
		})
	}()
	<-locked
	return func() {
		close(done)
		require.NoError(t, <-finished)
	}
}

func requireRotationInFlight(t *testing.T, err error) {
	t.Helper()
	var de *domain.Error
	require.ErrorAs(t, err, &de, "55P03 must reach the caller as a domain error, never a raw PgError (500)")
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
	assert.Equal(t, 409, de.Status())
}

// TestLockTimeout_MapsToRotationInFlight: a lock wait that exceeds
// lock_timeout on a principal row another credential write holds is the
// retryable 409 rotation_in_flight on every path that can block on it —
// LockForUpdate (TS-1/TS-2), LockByTenant (offboarding cascade; pgx v5
// reports its 55P03 from rows.Err(), not Query), and TS-4's Register
// (ON CONFLICT DO UPDATE on the locked row).
func TestLockTimeout_MapsToRotationInFlight(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	appPool := shortLockTimeoutAppPool(t, rawPool)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	tctx := withTenant(ctx, tenantID)

	principals := pgadapter.NewPrincipalRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	release := holdPrincipalLock(t, rawPool, tenantID)
	defer release()

	t.Run("LockForUpdate", func(t *testing.T) {
		err := txRunner.RunInTx(tctx, func(ctx context.Context) error {
			_, err := principals.LockForUpdate(ctx, tenantID, principalID)
			return err
		})
		requireRotationInFlight(t, err)
	})
	t.Run("LockByTenant", func(t *testing.T) {
		err := txRunner.RunInTx(tctx, func(ctx context.Context) error {
			_, err := principals.LockByTenant(ctx, tenantID)
			return err
		})
		requireRotationInFlight(t, err)
	})
	t.Run("Register under contention", func(t *testing.T) {
		_, _, _, err := principals.Register(tctx, &domain.ServiceAccountPrincipal{
			TenantID: tenantID, PrincipalSub: uuid.New(), KeycloakClientID: "platform-automation-changed",
			PrincipalType: domain.PrincipalTypePlatformAutomation,
		})
		requireRotationInFlight(t, err)
	})
}

// TestLockTimeout_ServiceAttachesActiveRotationID: through the real service
// and pool, a TS-1/TS-2 that times out on the principal lock still carries
// §17's details.active_rotation_id, read without a lock outside the aborted
// transaction (TS-D23).
func TestLockTimeout_ServiceAttachesActiveRotationID(t *testing.T) {
	t.Parallel()
	_, _, rawPool := setupTestDB(t)
	appPool := shortLockTimeoutAppPool(t, rawPool)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	tctx := withTenant(ctx, tenantID)

	svc := service.NewCredentialService(
		pgadapter.NewPrincipalRepository(appPool), pgadapter.NewCredentialRepository(appPool),
		newMemSecretStore(), pgadapter.NewTxRunner(appPool, nil), nil,
		func() (string, error) { return uuid.NewString(), nil })
	first := uuid.New()
	issued, err := svc.IssueOrRotate(tctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: first}, domain.SystemPrincipalID)
	require.NoError(t, err)

	release := holdPrincipalLock(t, rawPool, tenantID)
	defer release()

	_, err = svc.IssueOrRotate(tctx, tenantID, principalID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	requireRotationInFlight(t, err)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, first, de.Details["active_rotation_id"], "TS-1")

	_, err = svc.Revoke(tctx, tenantID, principalID, issued.Version, domain.SystemPrincipalID)
	requireRotationInFlight(t, err)
	require.ErrorAs(t, err, &de)
	assert.Equal(t, first, de.Details["active_rotation_id"], "TS-2")
}
