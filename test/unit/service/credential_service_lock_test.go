package service_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/service"
)

// inTxRunner is fakeTxRunner that also reports whether a call is currently
// inside RunInTx — i.e. while the principal row lock would be held.
type inTxRunner struct {
	fakeTxRunner
	inTx bool
}

func (r *inTxRunner) RunInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	r.inTx = true
	defer func() { r.inTx = false }()
	return r.fakeTxRunner.RunInTx(ctx, fn)
}

// deadlineSecretStore records the deadline of the ctx each Write runs under.
type deadlineSecretStore struct {
	*fakeSecretStore
	writeDeadline time.Time
	hadDeadline   bool
}

func (d *deadlineSecretStore) Write(ctx context.Context, path, secret string) error {
	d.writeDeadline, d.hadDeadline = ctx.Deadline()
	return d.fakeSecretStore.Write(ctx, path, secret)
}

// TS-1 must not hold the principal row lock across RSA keygen: other writers
// for the principal give up after PG_LOCK_TIMEOUT.
func TestCredentialService_IssueOrRotate_KeygenRunsBeforeTheLockedTx(t *testing.T) {
	principals := newFakePrincipalRepository()
	runner := &inTxRunner{}
	generatedInTx := true
	generate := func() (string, error) {
		generatedInTx = runner.inTx
		return uuid.NewString(), nil
	}
	svc := service.NewCredentialService(principals, newFakeCredentialRepository(), newFakeSecretStore(), runner, nil, generate)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	_, err := svc.IssueOrRotate(context.Background(), tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.False(t, generatedInTx, "keygen must run outside RunInTx")
}

// The in-lock OpenBao write (re-login included) is time-bounded even when
// the request itself has no deadline.
func TestCredentialService_IssueOrRotate_InLockSecretWriteIsBounded(t *testing.T) {
	principals := newFakePrincipalRepository()
	secrets := &deadlineSecretStore{fakeSecretStore: newFakeSecretStore()}
	svc := service.NewCredentialService(principals, newFakeCredentialRepository(), secrets, &fakeTxRunner{}, nil,
		func() (string, error) { return uuid.NewString(), nil })
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)

	start := time.Now()
	_, err := svc.IssueOrRotate(context.Background(), tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)
	require.True(t, secrets.hadDeadline, "the OpenBao write under the principal lock must carry a deadline")
	assert.LessOrEqual(t, secrets.writeDeadline.Sub(start), 5*time.Second+time.Second)
}

func issueThenAgeIssuedAt(t *testing.T, svc *service.CredentialService, principals *fakePrincipalRepository, credentials *fakeCredentialRepository, age time.Duration) (tenantID, principalID, rotationID uuid.UUID, version int) {
	t.Helper()
	tenantID = uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	rotationID = uuid.New()
	res, err := svc.IssueOrRotate(context.Background(), tenantID, p.ID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)
	for _, c := range credentials.byID {
		if c.PrincipalID == p.ID && c.Version == res.Version {
			c.IssuedAt = time.Now().UTC().Add(-age)
		}
	}
	return tenantID, p.ID, rotationID, res.Version
}

// A rotation_id replay past the window (default 15m) no longer returns the
// private key: 409 credential_replay_expired with details.version.
func TestCredentialService_IssueOrRotate_ReplayAfterWindowIsExpired(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	svc := service.NewCredentialService(principals, credentials, newFakeSecretStore(), &fakeTxRunner{}, nil,
		func() (string, error) { return uuid.NewString(), nil })
	tenantID, principalID, rotationID, version := issueThenAgeIssuedAt(t, svc, principals, credentials, service.DefaultReplayWindow+time.Minute)

	res, err := svc.IssueOrRotate(context.Background(), tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	assert.Nil(t, res)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrCredentialReplayExpired, de.Code)
	assert.Equal(t, 409, de.Status())
	assert.Equal(t, version, de.Details["version"])
}

func TestCredentialService_IssueOrRotate_ReplayInsideWindowStillReplays(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	svc := service.NewCredentialService(principals, credentials, newFakeSecretStore(), &fakeTxRunner{}, nil,
		func() (string, error) { return uuid.NewString(), nil }).WithReplayWindow(time.Hour)
	tenantID, principalID, rotationID, _ := issueThenAgeIssuedAt(t, svc, principals, credentials, 30*time.Minute)

	res, err := svc.IssueOrRotate(context.Background(), tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)
	assert.True(t, res.Replayed)
}

func TestCredentialService_WithReplayWindowZeroIsUnlimited(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		principals := newFakePrincipalRepository()
		credentials := newFakeCredentialRepository()
		svc := service.NewCredentialService(principals, credentials, newFakeSecretStore(), &fakeTxRunner{}, nil,
			func() (string, error) { return uuid.NewString(), nil }).WithReplayWindow(d)
		tenantID, principalID, rotationID, _ := issueThenAgeIssuedAt(t, svc, principals, credentials, 365*24*time.Hour)

		res, err := svc.IssueOrRotate(context.Background(), tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
		require.NoError(t, err, "window %v", d)
		assert.True(t, res.Replayed)
	}
}

// A revoked credential is still reported as revoked, not expired, even past
// the window — "gone" outranks "too late".
func TestCredentialService_IssueOrRotate_RevokedOutranksExpired(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	svc := service.NewCredentialService(principals, credentials, newFakeSecretStore(), &fakeTxRunner{}, nil,
		func() (string, error) { return uuid.NewString(), nil })
	tenantID, principalID, rotationID, version := issueThenAgeIssuedAt(t, svc, principals, credentials, time.Hour)
	_, err := svc.Revoke(context.Background(), tenantID, principalID, version, domain.SystemPrincipalID)
	require.NoError(t, err)

	_, err = svc.IssueOrRotate(context.Background(), tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrCredentialReplayRevoked, de.Code)
}

// lockTimeout is what the postgres adapter returns for SQLSTATE 55P03 on
// the principal row lock: rotation_in_flight with no details.
func lockTimeout() error {
	return domain.NewError(domain.ErrRotationInFlight, "another credential write for this principal is in progress; retry")
}

// §17's frozen details.active_rotation_id is attached to a lock-timeout
// rotation_in_flight (TS-1 and TS-2) from an unlocked read of the active
// credential.
func TestCredentialService_LockTimeoutRotationInFlightCarriesActiveRotationID(t *testing.T) {
	svc, principals, _, _, _ := newTestCredentialService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	first := uuid.New()
	issued, err := svc.IssueOrRotate(context.Background(), tenantID, p.ID, service.IssueOrRotateRequest{RotationID: first}, domain.SystemPrincipalID)
	require.NoError(t, err)

	principals.forceLockErr = lockTimeout()

	_, err = svc.IssueOrRotate(context.Background(), tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
	assert.Equal(t, first, de.Details["active_rotation_id"], "TS-1")

	_, err = svc.Revoke(context.Background(), tenantID, p.ID, issued.Version, domain.SystemPrincipalID)
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
	assert.Equal(t, first, de.Details["active_rotation_id"], "TS-2")
}

// No active credential (first issue racing another first issue): the error
// is returned as-is, without details.
func TestCredentialService_LockTimeoutWithoutActiveCredentialHasNoDetails(t *testing.T) {
	svc, principals, _, _, _ := newTestCredentialService(t)
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	principals.forceLockErr = lockTimeout()

	_, err := svc.IssueOrRotate(context.Background(), tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
	assert.NotContains(t, de.Details, "active_rotation_id")
}

// deleteDeadlineSecretStore records the deadline of the ctx Delete runs under.
type deleteDeadlineSecretStore struct {
	*fakeSecretStore
	deleteDeadline time.Time
	hadDeadline    bool
}

func (d *deleteDeadlineSecretStore) Delete(ctx context.Context, path string) error {
	d.deleteDeadline, d.hadDeadline = ctx.Deadline()
	return d.fakeSecretStore.Delete(ctx, path)
}

// TS-2's in-lock OpenBao delete is bounded like TS-1's write.
func TestCredentialService_Revoke_InLockSecretDeleteIsBounded(t *testing.T) {
	principals := newFakePrincipalRepository()
	secrets := &deleteDeadlineSecretStore{fakeSecretStore: newFakeSecretStore()}
	svc := service.NewCredentialService(principals, newFakeCredentialRepository(), secrets, &fakeTxRunner{}, nil,
		func() (string, error) { return uuid.NewString(), nil })
	tenantID := uuid.New()
	p := seedPrincipal(t, principals, tenantID)
	issued, err := svc.IssueOrRotate(context.Background(), tenantID, p.ID, service.IssueOrRotateRequest{RotationID: uuid.New()}, domain.SystemPrincipalID)
	require.NoError(t, err)

	start := time.Now()
	_, err = svc.Revoke(context.Background(), tenantID, p.ID, issued.Version, domain.SystemPrincipalID)
	require.NoError(t, err)
	require.True(t, secrets.hadDeadline, "the OpenBao delete under the principal lock must carry a deadline")
	assert.LessOrEqual(t, secrets.deleteDeadline.Sub(start), 5*time.Second+time.Second)
}

// Every rotation_id replay is an Info audit line with the identifiers and
// the outcome — and never the key (TS-D23).
func TestCredentialService_ReplayIsAudited(t *testing.T) {
	principals := newFakePrincipalRepository()
	credentials := newFakeCredentialRepository()
	log := &fakeLogger{}
	key := "-----BEGIN RSA PRIVATE KEY-----\nsecret-material\n-----END RSA PRIVATE KEY-----\n"
	svc := service.NewCredentialService(principals, credentials, newFakeSecretStore(), &fakeTxRunner{}, log,
		func() (string, error) { return key, nil })
	tenantID, principalID, rotationID, version := issueThenAgeIssuedAt(t, svc, principals, credentials, 0)

	_, err := svc.IssueOrRotate(context.Background(), tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	require.NoError(t, err)
	_, err = svc.Revoke(context.Background(), tenantID, principalID, version, domain.SystemPrincipalID)
	require.NoError(t, err)
	_, err = svc.IssueOrRotate(context.Background(), tenantID, principalID, service.IssueOrRotateRequest{RotationID: rotationID}, domain.SystemPrincipalID)
	require.Error(t, err)

	var results []any
	for _, call := range log.infos {
		if call.msg != "credential: rotation_id replay" {
			continue
		}
		results = append(results, call.fields["result"])
		assert.Equal(t, tenantID.String(), call.fields["tenant_id"])
		assert.Equal(t, principalID.String(), call.fields["principal_id"])
		assert.Equal(t, version, call.fields["version"])
		assert.Equal(t, rotationID.String(), call.fields["rotation_id"])
		for k, v := range call.fields {
			assert.NotContains(t, fmt.Sprint(v), "secret-material", "field %s leaks the key", k)
		}
	}
	assert.Equal(t, []any{"served", "revoked"}, results)
}
