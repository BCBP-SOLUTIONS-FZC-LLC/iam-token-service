//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrincipalRepository_RegisterIsIdempotent — TS-4: a repeat Register
// for the same (tenant_id, principal_type) with an unchanged
// principal_sub/keycloak_client_id is a true no-op — it returns the
// existing row with created=false, updated=false, never a duplicate
// (§5.4, uq_sap_active_principal).
func TestPrincipalRepository_RegisterIsIdempotent(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	repo := pgadapter.NewPrincipalRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	first := &domain.ServiceAccountPrincipal{
		TenantID:         tenantID,
		PrincipalSub:     uuid.New(),
		KeycloakClientID: testKeycloakClientID,
		PrincipalType:    domain.PrincipalTypePlatformAutomation,
	}

	var created1, created2 *domain.ServiceAccountPrincipal
	var wasCreated1, wasCreated2, wasUpdated1, wasUpdated2 bool
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		created1, wasCreated1, wasUpdated1, err = repo.Register(ctx, first)
		return err
	})
	require.NoError(t, err)
	assert.True(t, wasCreated1)
	assert.False(t, wasUpdated1)
	require.NotNil(t, created1)
	assert.Equal(t, tenantID, created1.TenantID)

	// Repeat with the identical principal_sub/keycloak_client_id — a true
	// no-op replay (e.g. a lost-response retry of the same RP-1/RP-2 call).
	second := &domain.ServiceAccountPrincipal{
		TenantID:         tenantID,
		PrincipalSub:     first.PrincipalSub,
		KeycloakClientID: testKeycloakClientID,
		PrincipalType:    domain.PrincipalTypePlatformAutomation,
	}
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		created2, wasCreated2, wasUpdated2, err = repo.Register(ctx, second)
		return err
	})
	require.NoError(t, err)
	assert.False(t, wasCreated2, "a repeat register must not create a second row")
	assert.False(t, wasUpdated2, "an identical repeat must not be reported as an update")
	require.NotNil(t, created2)
	assert.Equal(t, created1.ID, created2.ID, "repeat register must return the original principal")
	assert.Equal(t, created1.PrincipalSub, created2.PrincipalSub, "an identical repeat must preserve principal_sub")
}

// TestPrincipalRepository_RegisterUpdatesOnCarryOver — RP-3 conversion:
// re-registering (tenant_id, principal_type) with a DIFFERENT
// principal_sub/keycloak_client_id (a newly-minted dedicated-realm
// Keycloak client replacing the trial-realm one) updates the existing
// row in place rather than silently keeping the stale identity.
func TestPrincipalRepository_RegisterUpdatesOnCarryOver(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	repo := pgadapter.NewPrincipalRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	first := &domain.ServiceAccountPrincipal{
		TenantID:         tenantID,
		PrincipalSub:     uuid.New(),
		KeycloakClientID: domain.KeycloakClientPlatformAutomation + "-" + tenantID.String(),
		PrincipalType:    domain.PrincipalTypePlatformAutomation,
	}
	var created1 *domain.ServiceAccountPrincipal
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		created1, _, _, err = repo.Register(ctx, first)
		return err
	})
	require.NoError(t, err)

	second := &domain.ServiceAccountPrincipal{
		TenantID:         tenantID,
		PrincipalSub:     uuid.New(),
		KeycloakClientID: domain.KeycloakClientPlatformAutomation,
		PrincipalType:    domain.PrincipalTypePlatformAutomation,
	}
	var created2 *domain.ServiceAccountPrincipal
	var wasCreated2, wasUpdated2 bool
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		created2, wasCreated2, wasUpdated2, err = repo.Register(ctx, second)
		return err
	})
	require.NoError(t, err)
	assert.False(t, wasCreated2, "a carry-over must not report as a fresh create")
	assert.True(t, wasUpdated2, "a differing principal_sub/keycloak_client_id must be reported as an update")
	require.NotNil(t, created2)
	assert.Equal(t, created1.ID, created2.ID, "carry-over must update the same principal row, not create a new one")
	assert.Equal(t, second.PrincipalSub, created2.PrincipalSub, "carry-over must adopt the new principal_sub")
	assert.Equal(t, second.KeycloakClientID, created2.KeycloakClientID, "carry-over must adopt the new keycloak_client_id")
}

// TestCredentialRepository_IssueRotateOverlapRevoke exercises the full
// version lifecycle: issue v1 -> rotate to v2 (demoting v1 to rotating) ->
// revoke v1 -> verify exactly one active version at every step (CONC-2).
func TestCredentialRepository_IssueRotateOverlapRevoke(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	repo := pgadapter.NewCredentialRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	// Issue v1.
	v1 := &domain.Credential{
		TenantID:    tenantID,
		PrincipalID: principalID,
		Version:     1,
		Status:      domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 1),
		GrantedBy:   domain.SystemPrincipalID,
	}
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Insert(ctx, v1)
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, v1.ID)
	assert.Equal(t, 1, v1.RecordVersion)

	var active *domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		active, err = repo.FindActive(ctx, tenantID, principalID)
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, 1, active.Version)

	// Rotate: demote v1 to rotating FIRST, then insert v2 as active.
	// uq_sac_one_active is a plain (non-deferrable) unique index, so it is
	// enforced per-statement, not at commit — inserting v2 as `active`
	// while v1 is still `active` would violate it even inside the same
	// transaction. This statement order is the one the service layer
	// (§9.3) must use.
	v2 := &domain.Credential{
		TenantID:    tenantID,
		PrincipalID: principalID,
		Version:     2,
		Status:      domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 2),
		GrantedBy:   domain.SystemPrincipalID,
	}
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		v1.Status = domain.CredentialStatusRotating
		if err := repo.Update(ctx, v1); err != nil {
			return err
		}
		return repo.Insert(ctx, v2)
	})
	require.NoError(t, err)

	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		active, err = repo.FindActive(ctx, tenantID, principalID)
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, active)
	assert.Equal(t, 2, active.Version, "the new version must be the sole active credential")

	var list []*domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		list, err = repo.ListByPrincipal(ctx, tenantID, principalID)
		return err
	})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, 2, list[0].Version, "ListByPrincipal must be newest-first")
	assert.Equal(t, domain.CredentialStatusRotating, list[1].Status)

	// Revoke v1.
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		v1.Status = domain.CredentialStatusRevoked
		return repo.Update(ctx, v1)
	})
	require.NoError(t, err)

	var revoked *domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		revoked, err = repo.FindByVersion(ctx, tenantID, principalID, 1)
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, revoked)
	assert.Equal(t, domain.CredentialStatusRevoked, revoked.Status)
}

// TestCredentialRepository_CadenceRoundTrip — §16 TSQ-6 Resolved:
// rotation_cadence_days/next_rotation_at survive an Insert+FindActive
// round trip, and a subsequent demote-to-rotating Update nulls both out
// (only the current `active` row is ever "due", §4.2). Regression
// coverage for scanCredential's column list actually matching its Scan
// destinations — a mismatch here fails every credential read, not just
// this one.
func TestCredentialRepository_CadenceRoundTrip(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	repo := pgadapter.NewCredentialRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	cadenceDays := 90
	nextRotationAt := time.Now().UTC().AddDate(0, 0, cadenceDays).Truncate(time.Microsecond)
	v1 := &domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusActive, OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 1),
		GrantedBy:           domain.SystemPrincipalID,
		RotationCadenceDays: &cadenceDays, NextRotationAt: &nextRotationAt,
	}
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Insert(ctx, v1)
	})
	require.NoError(t, err)

	var active *domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		active, err = repo.FindActive(ctx, tenantID, principalID)
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, active)
	require.NotNil(t, active.RotationCadenceDays)
	assert.Equal(t, cadenceDays, *active.RotationCadenceDays)
	require.NotNil(t, active.NextRotationAt)
	assert.WithinDuration(t, nextRotationAt, *active.NextRotationAt, time.Second)

	// Demote to rotating, clearing cadence state (the write the service
	// layer performs on rotate, §16 TSQ-6 Resolved).
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		active.Status = domain.CredentialStatusRotating
		active.RotationCadenceDays = nil
		active.NextRotationAt = nil
		return repo.Update(ctx, active)
	})
	require.NoError(t, err)

	var demoted *domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		demoted, err = repo.FindByVersion(ctx, tenantID, principalID, 1)
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, demoted)
	assert.Nil(t, demoted.RotationCadenceDays)
	assert.Nil(t, demoted.NextRotationAt)
}

// TestCredentialRepository_UpdateOptimisticLockConflict — a stale
// record_version is rejected, never silently overwritten (CONC-1).
func TestCredentialRepository_UpdateOptimisticLockConflict(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	repo := pgadapter.NewCredentialRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	c := &domain.Credential{
		TenantID:    tenantID,
		PrincipalID: principalID,
		Version:     1,
		Status:      domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 1),
		GrantedBy:   domain.SystemPrincipalID,
	}
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Insert(ctx, c)
	})
	require.NoError(t, err)

	stale := &domain.Credential{ID: c.ID, RecordVersion: c.RecordVersion + 99, Status: domain.CredentialStatusRevoked}
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Update(ctx, stale)
	})
	require.Error(t, err)
	domainErr, ok := err.(*domain.Error)
	require.True(t, ok, "expected a *domain.Error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrOptimisticLockConflict, domainErr.Code)
}

// TestCredentialRepository_RotationIDIdempotency — a replayed TS-1 call
// under the same rotation_id is discoverable via FindByRotationID without
// generating a second row (uq_sac_rotation_id, §9.2).
func TestCredentialRepository_RotationIDIdempotency(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	repo := pgadapter.NewCredentialRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)
	rotationID := uuid.New()

	c := &domain.Credential{
		TenantID:    tenantID,
		PrincipalID: principalID,
		Version:     1,
		Status:      domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 1),
		GrantedBy:   domain.SystemPrincipalID,
		RotationID:  &rotationID,
	}
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Insert(ctx, c)
	})
	require.NoError(t, err)

	var found *domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		found, err = repo.FindByRotationID(ctx, tenantID, principalID, rotationID)
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, c.ID, found.ID)

	// A second Insert under the same rotation_id must violate
	// uq_sac_rotation_id — the service layer is what actually prevents
	// this by checking FindByRotationID first, but the constraint is the
	// backstop.
	dup := &domain.Credential{
		TenantID:    tenantID,
		PrincipalID: principalID,
		Version:     2,
		Status:      domain.CredentialStatusActive,
		OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 2),
		GrantedBy:   domain.SystemPrincipalID,
		RotationID:  &rotationID,
	}
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Insert(ctx, dup)
	})
	require.Error(t, err, "a second row under the same rotation_id must be rejected (uq_sac_rotation_id)")
}

// TestProcessedEventsRepository_MarkAndCheck — the dedup round trip
// (§9.2, ON CONFLICT DO NOTHING at the DB level).
func TestProcessedEventsRepository_MarkAndCheck(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()
	// processed_events is RLS-exempt (§4.3) — no tenant GUC needed, the
	// plain appPool (no WithGUCSet in ctx) reaches it directly.
	repo := pgadapter.NewProcessedEventsRepository(appPool)

	eventID := uuid.NewString()

	seen, err := repo.IsProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, eventID)
	require.NoError(t, err)
	assert.False(t, seen)

	require.NoError(t, repo.MarkProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, eventID))

	seen, err = repo.IsProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, eventID)
	require.NoError(t, err)
	assert.True(t, seen)

	// Idempotent re-mark: no error, no duplicate row (composite PK).
	require.NoError(t, repo.MarkProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, eventID))
}

// TestPrincipalRepository_FindByID covers the found, not-found, and
// cross-tenant-invisible branches — the latter two surface the identical
// domain.ErrPrincipalNotFound (RLS makes a cross-tenant row indistinguishable
// from an absent one, §5.5).
func TestPrincipalRepository_FindByID(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	repo := pgadapter.NewPrincipalRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	var found *domain.ServiceAccountPrincipal
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		found, err = repo.FindByID(ctx, tenantID, principalID)
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, principalID, found.ID)
	assert.Equal(t, tenantID, found.TenantID)

	// Not found: a random id under the correct tenant GUC.
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		_, err := repo.FindByID(ctx, tenantID, uuid.New())
		return err
	})
	require.Error(t, err)
	domainErr, ok := err.(*domain.Error)
	require.True(t, ok, "expected *domain.Error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrPrincipalNotFound, domainErr.Code)

	// Cross-tenant invisible: the real principal id, but a caller GUC bound
	// to a different tenant — RLS hides it, surfacing the same not-found
	// error as a genuinely absent id.
	otherTenant := uuid.New()
	err = txRunner.RunInTx(withTenant(ctx, otherTenant), func(ctx context.Context) error {
		_, err := repo.FindByID(ctx, tenantID, principalID)
		return err
	})
	require.Error(t, err)
	domainErr, ok = err.(*domain.Error)
	require.True(t, ok, "expected *domain.Error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrPrincipalNotFound, domainErr.Code)
}

// TestPrincipalRepository_FindByPrincipalSub covers the same found,
// not-found, and cross-tenant-invisible branches as FindByID, but keyed by
// the Keycloak sub (TS-5, AUTH-9) — the identifier org-membership's
// service-account-not-grantable check actually has, since it never sees
// Token Service's own internal principal id.
func TestPrincipalRepository_FindByPrincipalSub(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalSub := uuid.New()
	principalID := uuid.New()
	_, err := rawPool.Exec(ctx, `
		INSERT INTO service_account_principals (id, tenant_id, principal_sub, keycloak_client_id)
		VALUES ($1, $2, $3, $4)`, principalID, tenantID, principalSub, testKeycloakClientID)
	require.NoError(t, err)
	repo := pgadapter.NewPrincipalRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	var found *domain.ServiceAccountPrincipal
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		found, err = repo.FindByPrincipalSub(ctx, tenantID, principalSub)
		return err
	})
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, principalID, found.ID)
	assert.Equal(t, tenantID, found.TenantID)
	assert.Equal(t, principalSub, found.PrincipalSub)

	// Not found: a random sub under the correct tenant GUC — the expected,
	// common case for every real human user checked.
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		_, err := repo.FindByPrincipalSub(ctx, tenantID, uuid.New())
		return err
	})
	require.Error(t, err)
	domainErr, ok := err.(*domain.Error)
	require.True(t, ok, "expected *domain.Error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrPrincipalNotFound, domainErr.Code)

	// Cross-tenant invisible: the real principal_sub, but a caller GUC bound
	// to a different tenant — RLS hides it (§5.5).
	otherTenant := uuid.New()
	err = txRunner.RunInTx(withTenant(ctx, otherTenant), func(ctx context.Context) error {
		_, err := repo.FindByPrincipalSub(ctx, tenantID, principalSub)
		return err
	})
	require.Error(t, err)
	domainErr, ok = err.(*domain.Error)
	require.True(t, ok, "expected *domain.Error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrPrincipalNotFound, domainErr.Code)
}

// TestPrincipalRepository_FindByType covers TS-6 (TS-D17): the tenant's
// automation principal resolves from the tenant id alone; an RP-3/RP-4
// re-mint carry-over changes the sub it returns but not principal_id; a
// tenant with none, or a caller GUC bound to another tenant, is
// principal_not_found.
func TestPrincipalRepository_FindByType(t *testing.T) {
	t.Parallel()
	appPool, _, _ := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	repo := pgadapter.NewPrincipalRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	find := func(guc, tenant uuid.UUID) (*domain.ServiceAccountPrincipal, error) {
		var found *domain.ServiceAccountPrincipal
		err := txRunner.RunInTx(withTenant(ctx, guc), func(ctx context.Context) error {
			var err error
			found, err = repo.FindByType(ctx, tenant, domain.PrincipalTypePlatformAutomation)
			return err
		})
		return found, err
	}
	register := func(sub uuid.UUID, clientID string) *domain.ServiceAccountPrincipal {
		var out *domain.ServiceAccountPrincipal
		require.NoError(t, txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
			var err error
			out, _, _, err = repo.Register(ctx, &domain.ServiceAccountPrincipal{
				TenantID: tenantID, PrincipalSub: sub, KeycloakClientID: clientID,
				PrincipalType: domain.PrincipalTypePlatformAutomation,
			})
			return err
		}))
		return out
	}
	assertNotFound := func(err error) {
		t.Helper()
		require.Error(t, err)
		domainErr, ok := err.(*domain.Error)
		require.True(t, ok, "expected *domain.Error, got %T: %v", err, err)
		assert.Equal(t, domain.ErrPrincipalNotFound, domainErr.Code)
	}

	_, err := find(tenantID, tenantID)
	assertNotFound(err)

	trialSub := uuid.New()
	minted := register(trialSub, domain.KeycloakClientPlatformAutomation+"-"+tenantID.String())
	found, err := find(tenantID, tenantID)
	require.NoError(t, err)
	assert.Equal(t, minted.ID, found.ID)
	assert.Equal(t, trialSub, found.PrincipalSub)

	// RP-3 convert re-mints in the dedicated realm: new sub, same principal.
	dedicatedSub := uuid.New()
	register(dedicatedSub, domain.KeycloakClientPlatformAutomation)
	found, err = find(tenantID, tenantID)
	require.NoError(t, err)
	assert.Equal(t, minted.ID, found.ID, "principal_id must survive a re-mint")
	assert.Equal(t, dedicatedSub, found.PrincipalSub, "principal_sub follows the re-mint")

	// Cross-tenant invisible under RLS (§5.5).
	_, err = find(uuid.New(), tenantID)
	assertNotFound(err)
}

// TestCredentialRepository_FindNotFoundReturnsNilNotError — FindByVersion,
// FindActive, and FindByRotationID all return (nil, nil) — not an error —
// when no matching row exists, mirroring the in-memory fake's behavior
// (test/unit/service/fakes_test.go) that CredentialService's business logic
// (e.g. the "no active credential yet" issue path) depends on.
func TestCredentialRepository_FindNotFoundReturnsNilNotError(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	repo := pgadapter.NewCredentialRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	var byVersion *domain.Credential
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		byVersion, err = repo.FindByVersion(ctx, tenantID, principalID, 1)
		return err
	})
	require.NoError(t, err)
	assert.Nil(t, byVersion)

	var active *domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		active, err = repo.FindActive(ctx, tenantID, principalID)
		return err
	})
	require.NoError(t, err)
	assert.Nil(t, active)

	var byRotation *domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		byRotation, err = repo.FindByRotationID(ctx, tenantID, principalID, uuid.New())
		return err
	})
	require.NoError(t, err)
	assert.Nil(t, byRotation)
}

// TestCredentialRepository_InsertConflicts exercises Insert's 23505 ->
// domain.ErrRotationInFlight mapping directly (not merely the
// service-layer-guarded path), covering both the uq_sac_one_active branch
// (a second `active` row, activeRotationID's "present" branch since the
// existing active row DOES carry a rotation_id) and the uq_sac_version
// branch on a non-active row (activeRotationID's "nil" branch, since no
// `active` row exists at all at that point).
func TestCredentialRepository_InsertConflicts(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	repo := pgadapter.NewCredentialRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	// uq_sac_one_active: a second `active` row while one is already active.
	// The existing active row carries a rotation_id, so Insert's error path
	// must resolve activeRotationID to a non-nil id.
	activeRotationID := uuid.New()
	first := &domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 1,
		Status: domain.CredentialStatusActive, OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 1),
		GrantedBy: domain.SystemPrincipalID, RotationID: &activeRotationID,
	}
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Insert(ctx, first)
	})
	require.NoError(t, err)

	secondActive := &domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 2,
		Status: domain.CredentialStatusActive, OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 2),
		GrantedBy: domain.SystemPrincipalID,
	}
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Insert(ctx, secondActive)
	})
	require.Error(t, err)
	domainErr, ok := err.(*domain.Error)
	require.True(t, ok, "expected *domain.Error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrRotationInFlight, domainErr.Code)
	require.NotNil(t, domainErr.Details)
	assert.Equal(t, activeRotationID, domainErr.Details["active_rotation_id"],
		"activeRotationID must resolve to the currently-active row's rotation_id")

	// Demote the active row so a second principal can exercise the
	// no-active-row branch (activeRotationID returns nil, nil): a
	// uq_sac_version conflict where nothing is `active` at insert time.
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		first.Status = domain.CredentialStatusRotating
		return repo.Update(ctx, first)
	})
	require.NoError(t, err)

	dupVersion := &domain.Credential{
		TenantID: tenantID, PrincipalID: principalID, Version: 1, // same version as `first`
		Status: domain.CredentialStatusRotating, OpenBaoPath: domain.OpenBaoPathFor(tenantID, testKeycloakClientID, 1),
		GrantedBy: domain.SystemPrincipalID,
	}
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return repo.Insert(ctx, dupVersion)
	})
	require.Error(t, err)
	domainErr, ok = err.(*domain.Error)
	require.True(t, ok, "expected *domain.Error, got %T: %v", err, err)
	assert.Equal(t, domain.ErrRotationInFlight, domainErr.Code)
	assert.Nil(t, domainErr.Details["active_rotation_id"],
		"no active row exists at this point — activeRotationID must resolve to nil, leaving details unset")
}

// TestProcessedEventsRepository_Prune — deletes only rows older than
// ttlDays, respecting the limit batch size so a backlog larger than one
// tick converges over several calls (§4.2/§15.4).
func TestProcessedEventsRepository_Prune(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	repo := pgadapter.NewProcessedEventsRepository(appPool)

	// Three old rows (eligible) + one fresh row (not eligible).
	var oldIDs []string
	for i := 0; i < 3; i++ {
		id := uuid.NewString()
		oldIDs = append(oldIDs, id)
		_, err := rawPool.Exec(ctx, `
			INSERT INTO processed_events (event_id, consumer, processed_at)
			VALUES ($1, $2, now() - interval '10 days')`, id, string(port.ProcessedEventsConsumerTenantOffboarding))
		require.NoError(t, err)
	}
	freshID := uuid.NewString()
	require.NoError(t, repo.MarkProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, freshID))

	// No-op: nothing older than a very long TTL.
	n, err := repo.Prune(ctx, 365, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// First call: TTL of 8 days makes the 3 old rows eligible, but limit=2
	// caps this call to 2 deletions.
	n, err = repo.Prune(ctx, 8, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "must respect the limit batch size")

	// Second call: the remaining 1 old row is pruned; the fresh row is untouched.
	n, err = repo.Prune(ctx, 8, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	seenFresh, err := repo.IsProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, freshID)
	require.NoError(t, err)
	assert.True(t, seenFresh, "a fresh row must survive pruning")

	for _, id := range oldIDs {
		seen, err := repo.IsProcessed(ctx, port.ProcessedEventsConsumerTenantOffboarding, id)
		require.NoError(t, err)
		assert.False(t, seen, "an old row must be gone after pruning")
	}
}

// TestPrincipalRepository_ListByTenantAndDeleteByTenant — the offboarding
// cascade's read-then-delete path (§8.4): ListByTenant discovers the
// tenant's principal(s) so their credentials' OpenBao paths can be
// reclaimed, then DeleteByTenant hard-deletes the principal and (via the
// composite FK's ON DELETE CASCADE) its credential rows.
func TestPrincipalRepository_ListByTenantAndDeleteByTenant(t *testing.T) {
	t.Parallel()
	appPool, _, rawPool := setupTestDB(t)
	ctx := context.Background()
	tenantID := uuid.New()
	principalID := seedPrincipal(t, ctx, rawPool, tenantID)
	seedCredential(t, ctx, rawPool, tenantID, principalID, 1, "active")

	principalRepo := pgadapter.NewPrincipalRepository(appPool)
	credentialRepo := pgadapter.NewCredentialRepository(appPool)
	txRunner := pgadapter.NewTxRunner(appPool, nil)

	var listed []*domain.ServiceAccountPrincipal
	err := txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		listed, err = principalRepo.ListByTenant(ctx, tenantID)
		return err
	})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, principalID, listed[0].ID)

	var creds []*domain.Credential
	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		creds, err = credentialRepo.ListByPrincipal(ctx, tenantID, principalID)
		return err
	})
	require.NoError(t, err)
	require.Len(t, creds, 1)

	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		return principalRepo.DeleteByTenant(ctx, tenantID)
	})
	require.NoError(t, err)

	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		listed, err = principalRepo.ListByTenant(ctx, tenantID)
		return err
	})
	require.NoError(t, err)
	assert.Empty(t, listed, "principal must be gone after DeleteByTenant")

	err = txRunner.RunInTx(withTenant(ctx, tenantID), func(ctx context.Context) error {
		var err error
		creds, err = credentialRepo.ListByPrincipal(ctx, tenantID, principalID)
		return err
	})
	require.NoError(t, err)
	assert.Empty(t, creds, "credential rows must cascade-delete with their principal")
}
