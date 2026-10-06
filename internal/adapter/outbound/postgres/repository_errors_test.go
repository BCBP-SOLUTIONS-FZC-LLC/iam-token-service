package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// ─────────────────────────────────────────────────────────────────────────
// withPool joins whatever *pgx.Tx is already bound to ctx (port.TxFromContext)
// instead of opening a real pool connection — the exact seam repository
// unit tests here use to force every driver-error branch (non-ErrNoRows scan
// failures, tx.Query/tx.Exec failures, SAVEPOINT/ROLLBACK failures) without a
// live Postgres. The happy paths and the ErrNoRows branches are already
// proven against a real database by test/postgres; these tests cover the
// branches that would otherwise require deliberately breaking a live
// connection mid-transaction.
// ─────────────────────────────────────────────────────────────────────────

// errRow is a pgx.Row whose Scan always returns err.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// oneRowThenScanErrRows is a pgx.Rows that yields exactly one row and then
// fails on Scan — covering a repository's per-row scan-error branch inside
// its rows.Next() loop.
type oneRowThenScanErrRows struct {
	pgx.Rows
	yielded bool
	scanErr error
}

func (r *oneRowThenScanErrRows) Next() bool {
	if r.yielded {
		return false
	}
	r.yielded = true
	return true
}
func (r *oneRowThenScanErrRows) Scan(...any) error { return r.scanErr }
func (r *oneRowThenScanErrRows) Err() error        { return nil }
func (r *oneRowThenScanErrRows) Close()            {}

// fakeRepoTx is a minimal pgx.Tx driving only QueryRow/Query/Exec — the only
// methods every repository in this package calls. Any other method panics
// via the embedded nil pgx.Tx, which is fine: none of these tests reach one.
type fakeRepoTx struct {
	pgx.Tx

	queryRowResults []pgx.Row // popped in call order; last entry repeats once exhausted
	queryErr        error     // returned by every Query call when set
	queryRows       pgx.Rows  // returned by Query when queryErr is nil
	execErrFor      func(sql string) error
}

func (f *fakeRepoTx) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	if len(f.queryRowResults) == 0 {
		return errRow{err: errors.New("fakeRepoTx: no QueryRow result queued")}
	}
	row := f.queryRowResults[0]
	if len(f.queryRowResults) > 1 {
		f.queryRowResults = f.queryRowResults[1:]
	}
	return row
}

func (f *fakeRepoTx) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.queryRows, nil
}

func (f *fakeRepoTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if f.execErrFor != nil {
		if err := f.execErrFor(sql); err != nil {
			return pgconn.CommandTag{}, err
		}
	}
	return pgconn.NewCommandTag("OK"), nil
}

func ctxWithFakeTx(tx pgx.Tx) context.Context {
	return port.WithTx(context.Background(), tx)
}

// ── credential_repository.go ──────────────────────────────────────────

func TestCredentialRepository_FindByVersion_ScanErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	got, err := r.FindByVersion(ctxWithFakeTx(tx), uuid.New(), uuid.New(), 1)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestCredentialRepository_FindActive_ScanErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	got, err := r.FindActive(ctxWithFakeTx(tx), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestCredentialRepository_FindByRotationID_ScanErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	got, err := r.FindByRotationID(ctxWithFakeTx(tx), uuid.New(), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestCredentialRepository_ListByPrincipal_QueryErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("query boom")
	tx := &fakeRepoTx{queryErr: wantErr}

	got, err := r.ListByPrincipal(ctxWithFakeTx(tx), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestCredentialRepository_ListByPrincipal_ScanErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRows: &oneRowThenScanErrRows{scanErr: wantErr}}

	got, err := r.ListByPrincipal(ctxWithFakeTx(tx), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestCredentialRepository_Insert_SavepointExecErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("savepoint boom")
	tx := &fakeRepoTx{execErrFor: func(sql string) error {
		if strings.Contains(sql, "SAVEPOINT") && !strings.Contains(sql, "ROLLBACK") && !strings.Contains(sql, "RELEASE") {
			return wantErr
		}
		return nil
	}}

	err := r.Insert(ctxWithFakeTx(tx), &domain.Credential{})
	require.ErrorIs(t, err, wantErr)
}

func TestCredentialRepository_Insert_UniqueViolationRollbackExecErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("rollback boom")
	tx := &fakeRepoTx{
		queryRowResults: []pgx.Row{errRow{err: &pgconn.PgError{Code: "23505"}}},
		execErrFor: func(sql string) error {
			if strings.Contains(sql, "ROLLBACK") {
				return wantErr
			}
			return nil
		},
	}

	err := r.Insert(ctxWithFakeTx(tx), &domain.Credential{})
	require.ErrorIs(t, err, wantErr)
}

func TestCredentialRepository_Insert_NonUniqueQueryErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("disk full")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	err := r.Insert(ctxWithFakeTx(tx), &domain.Credential{})
	require.ErrorIs(t, err, wantErr)
}

func TestActiveRotationID_ScanErrorPropagates(t *testing.T) {
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	id, err := activeRotationID(context.Background(), tx, uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, id)
}

func TestCredentialRepository_Update_NonOptimisticLockScanErrorPropagates(t *testing.T) {
	r := NewCredentialRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	err := r.Update(ctxWithFakeTx(tx), &domain.Credential{ID: uuid.New()})
	require.ErrorIs(t, err, wantErr)
	var de *domain.Error
	assert.False(t, errors.As(err, &de), "a non-ErrNoRows failure must not be misreported as an optimistic-lock conflict")
}

// ── principal_repository.go ───────────────────────────────────────────

func TestPrincipalRepository_FindByID_ScanErrorPropagates(t *testing.T) {
	r := NewPrincipalRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	got, err := r.FindByID(ctxWithFakeTx(tx), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestPrincipalRepository_FindByPrincipalSub_ScanErrorPropagates(t *testing.T) {
	r := NewPrincipalRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	got, err := r.FindByPrincipalSub(ctxWithFakeTx(tx), uuid.New(), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestPrincipalRepository_FindByType_ScanErrorPropagates(t *testing.T) {
	r := NewPrincipalRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	got, err := r.FindByType(ctxWithFakeTx(tx), uuid.New(), domain.PrincipalTypePlatformAutomation)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestPrincipalRepository_Register_InsertScanErrorPropagates(t *testing.T) {
	r := NewPrincipalRepository(nil)
	wantErr := errors.New("insert scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	got, created, updated, err := r.Register(ctxWithFakeTx(tx), &domain.ServiceAccountPrincipal{})
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
	assert.False(t, created)
	assert.False(t, updated)
}

func TestPrincipalRepository_Register_ConflictFallbackSelectScanErrorPropagates(t *testing.T) {
	r := NewPrincipalRepository(nil)
	wantErr := errors.New("fallback select boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{
		errRow{err: pgx.ErrNoRows},
		errRow{err: wantErr},
	}}

	got, created, updated, err := r.Register(ctxWithFakeTx(tx), &domain.ServiceAccountPrincipal{})
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
	assert.False(t, created)
	assert.False(t, updated)
}

func TestPrincipalRepository_ListByTenant_QueryErrorPropagates(t *testing.T) {
	r := NewPrincipalRepository(nil)
	wantErr := errors.New("query boom")
	tx := &fakeRepoTx{queryErr: wantErr}

	got, err := r.ListByTenant(ctxWithFakeTx(tx), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestPrincipalRepository_ListByTenant_ScanErrorPropagates(t *testing.T) {
	r := NewPrincipalRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRows: &oneRowThenScanErrRows{scanErr: wantErr}}

	got, err := r.ListByTenant(ctxWithFakeTx(tx), uuid.New())
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

// rowsErrRows is a pgx.Rows that yields no rows and reports err from Err()
// — how pgx v5 surfaces a FOR UPDATE lock wait that exceeded lock_timeout
// (Query only sends the statement; the 55P03 arrives with the result).
type rowsErrRows struct {
	pgx.Rows
	err error
}

func (r *rowsErrRows) Next() bool { return false }
func (r *rowsErrRows) Err() error { return r.err }
func (r *rowsErrRows) Close()     {}

func TestPrincipalRepository_LockByTenant_LockTimeoutFromRowsErrIsRotationInFlight(t *testing.T) {
	r := NewPrincipalRepository(nil)
	tx := &fakeRepoTx{queryRows: &rowsErrRows{err: &pgconn.PgError{Code: "55P03"}}}

	got, err := r.LockByTenant(ctxWithFakeTx(tx), uuid.New())
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
	assert.Nil(t, got)
}

func TestPrincipalRepository_LockForUpdate_LockTimeoutIsRotationInFlight(t *testing.T) {
	r := NewPrincipalRepository(nil)
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: &pgconn.PgError{Code: "55P03"}}}}

	_, err := r.LockForUpdate(ctxWithFakeTx(tx), uuid.New(), uuid.New())
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
}

func TestPrincipalRepository_Register_LockTimeoutIsRotationInFlight(t *testing.T) {
	r := NewPrincipalRepository(nil)
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: &pgconn.PgError{Code: "55P03"}}}}

	_, _, _, err := r.Register(ctxWithFakeTx(tx), &domain.ServiceAccountPrincipal{TenantID: uuid.New()})
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrRotationInFlight, de.Code)
}

// ── reconciler_repository.go ──────────────────────────────────────────

func TestReconcilerRepository_ListExpiredRotating_QueryErrorPropagates(t *testing.T) {
	r := NewReconcilerRepository(nil)
	wantErr := errors.New("query boom")
	tx := &fakeRepoTx{queryErr: wantErr}

	got, err := r.ListExpiredRotating(ctxWithFakeTx(tx), 100)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestReconcilerRepository_ListExpiredRotating_ScanErrorPropagates(t *testing.T) {
	r := NewReconcilerRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRows: &oneRowThenScanErrRows{scanErr: wantErr}}

	got, err := r.ListExpiredRotating(ctxWithFakeTx(tx), 100)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestReconcilerRepository_ListDueForRotation_QueryErrorPropagates(t *testing.T) {
	r := NewReconcilerRepository(nil)
	wantErr := errors.New("query boom")
	tx := &fakeRepoTx{queryErr: wantErr}

	got, err := r.ListDueForRotation(ctxWithFakeTx(tx), 100)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestReconcilerRepository_ListDueForRotation_ScanErrorPropagates(t *testing.T) {
	r := NewReconcilerRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRows: &oneRowThenScanErrRows{scanErr: wantErr}}

	got, err := r.ListDueForRotation(ctxWithFakeTx(tx), 100)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestReconcilerRepository_ListPrincipalMaterialStates_QueryErrorPropagates(t *testing.T) {
	r := NewReconcilerRepository(nil)
	wantErr := errors.New("query boom")
	tx := &fakeRepoTx{queryErr: wantErr}

	got, err := r.ListPrincipalMaterialStates(ctxWithFakeTx(tx))
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestReconcilerRepository_ListPrincipalMaterialStates_ScanErrorPropagates(t *testing.T) {
	r := NewReconcilerRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRows: &oneRowThenScanErrRows{scanErr: wantErr}}

	got, err := r.ListPrincipalMaterialStates(ctxWithFakeTx(tx))
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

// ── inbox_repository.go ───────────────────────────────────────────────
// The inbox.Store opens its own transaction on the pool (it does not join a
// ctx-bound pgx.Tx), so the happy and duplicate paths are covered against a
// real Postgres in test/postgres/consumer_test.go; here only the
// store-construction error is reachable without one.

func TestInboxRepository_NilPoolFailsBeforeRunningFn(t *testing.T) {
	r := NewInboxRepository(nil, nil)
	ran := false

	dup, err := r.ProcessOnce(context.Background(), port.ProcessedEventsConsumerTenantOffboarding, "0b5b6c3e-6f1e-4c55-9d6f-6b8f0c2b1a11", "TenantMembershipsPurged",
		func(context.Context) error { ran = true; return nil })
	require.Error(t, err)
	assert.False(t, dup)
	assert.False(t, ran, "fn must not run when the claim cannot be made")

	n, err := r.Prune(context.Background(), port.ProcessedEventsConsumerTenantOffboarding, time.Hour, 10)
	require.Error(t, err)
	assert.Zero(t, n)
}
