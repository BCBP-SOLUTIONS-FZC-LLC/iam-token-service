package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-token-service/internal/core/port"
)

// Driver-error and edge branches not reached by repository_errors_test.go,
// driven through the same fakeRepoTx seam (withPool joins the ctx's tx).

// scriptedRows is a pgx.Rows yielding one row per scan func, each of which
// fills the Scan destinations directly.
type scriptedRows struct {
	pgx.Rows
	scans []func(dest ...any) error
	i     int
}

func (r *scriptedRows) Next() bool {
	if r.i >= len(r.scans) {
		return false
	}
	r.i++
	return true
}
func (r *scriptedRows) Scan(dest ...any) error { return r.scans[r.i-1](dest...) }
func (r *scriptedRows) Err() error             { return nil }
func (r *scriptedRows) Close()                 {}

// ── principal_repository.go ───────────────────────────────────────────

func TestPrincipalRepository_LockForUpdate_NoRowsIsPrincipalNotFound(t *testing.T) {
	r := NewPrincipalRepository(nil)
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: pgx.ErrNoRows}}}

	got, err := r.LockForUpdate(ctxWithFakeTx(tx), uuid.New(), uuid.New())
	var de *domain.Error
	require.ErrorAs(t, err, &de)
	assert.Equal(t, domain.ErrPrincipalNotFound, de.Code)
	assert.Nil(t, got)
}

// ── reconciler_repository.go ──────────────────────────────────────────

// A credential row with a NULL openbao_path (a column the LEFT JOIN can
// leave NULL) maps to an empty OpenBaoPath via deref, not a panic.
func TestReconcilerRepository_ListPrincipalMaterialStates_NullPathDerefsToEmpty(t *testing.T) {
	r := NewReconcilerRepository(nil)
	tenantID, principalID := uuid.New(), uuid.New()
	tx := &fakeRepoTx{queryRows: &scriptedRows{scans: []func(dest ...any) error{
		func(dest ...any) error {
			*dest[0].(*uuid.UUID) = tenantID
			*dest[1].(*uuid.UUID) = principalID
			*dest[2].(*string) = "platform-automation"
			v := 3
			*dest[3].(**int) = &v
			*dest[4].(**string) = nil
			*dest[5].(**bool) = nil
			return nil
		},
	}}}

	got, err := r.ListPrincipalMaterialStates(ctxWithFakeTx(tx))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, tenantID, got[0].TenantID)
	assert.Equal(t, principalID, got[0].PrincipalID)
	assert.Equal(t, 3, got[0].MaxVersion)
	assert.Equal(t, []port.CredentialMaterial{{Version: 3, OpenBaoPath: "", Live: false}}, got[0].Credentials)
}

func TestDeref(t *testing.T) {
	s := "iam/x"
	assert.Equal(t, "iam/x", deref(&s))
	assert.Empty(t, deref(nil))
}

// ── jwks_tenants_repository.go ────────────────────────────────────────

func TestJWKSTenantsRepository_ListTenantsWithLiveCredentials_QueryErrorPropagates(t *testing.T) {
	r := NewJWKSTenantsRepository(nil)
	wantErr := errors.New("query boom")
	tx := &fakeRepoTx{queryErr: wantErr}

	got, err := r.ListTenantsWithLiveCredentials(ctxWithFakeTx(tx))
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestJWKSTenantsRepository_ListTenantsWithLiveCredentials_ScanErrorPropagates(t *testing.T) {
	r := NewJWKSTenantsRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRows: &oneRowThenScanErrRows{scanErr: wantErr}}

	got, err := r.ListTenantsWithLiveCredentials(ctxWithFakeTx(tx))
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

// ── rls_violation_repository.go ───────────────────────────────────────

func TestRLSViolationRepository_CountSince_QueryErrorKeepsCursor(t *testing.T) {
	r := NewRLSViolationRepository(nil)
	wantErr := errors.New("query boom")
	tx := &fakeRepoTx{queryErr: wantErr}

	counts, cursor, err := r.CountSince(ctxWithFakeTx(tx), 42)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, counts)
	assert.Equal(t, int64(42), cursor, "a failed read must not move the cursor")
}

func TestRLSViolationRepository_CountSince_ScanErrorKeepsCursor(t *testing.T) {
	r := NewRLSViolationRepository(nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRows: &oneRowThenScanErrRows{scanErr: wantErr}}

	counts, cursor, err := r.CountSince(ctxWithFakeTx(tx), 7)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, counts)
	assert.Equal(t, int64(7), cursor)
}
