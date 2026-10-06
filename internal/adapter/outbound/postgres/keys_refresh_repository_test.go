package postgres

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Driver-error branches of keys_refresh_repository.go, via the fakeRepoTx
// seam (repository_errors_test.go). The happy paths and the marker
// semantics (fresh vs. updated, Clear's upTo guard, intent vs. committed
// markers, PendingStats) are proven
// against a real Postgres by test/postgres/keys_refresh_test.go.

func TestKeysRefreshRepository_MarkPending_ScanErrorPropagates(t *testing.T) {
	r := NewKeysRefreshRepository(nil, nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	_, err := r.MarkPending(ctxWithFakeTx(tx), uuid.New())
	require.ErrorIs(t, err, wantErr)
}

func TestKeysRefreshRepository_Clear_ExecErrorPropagates(t *testing.T) {
	r := NewKeysRefreshRepository(nil, nil)
	wantErr := errors.New("exec boom")
	var gotSQL string
	tx := &fakeRepoTx{execErrFor: func(sql string) error { gotSQL = sql; return wantErr }}

	err := r.Clear(ctxWithFakeTx(tx), uuid.New(), time.Now())
	require.ErrorIs(t, err, wantErr)
	assert.True(t, strings.Contains(gotSQL, "requested_at <= $2"), "Clear must never delete a newer request")
}

func TestKeysRefreshRepository_ListPending_QueryErrorPropagates(t *testing.T) {
	r := NewKeysRefreshRepository(nil, nil)
	wantErr := errors.New("query boom")
	tx := &fakeRepoTx{queryErr: wantErr}

	got, err := r.ListPending(ctxWithFakeTx(tx), 10)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestKeysRefreshRepository_ListPending_ScanErrorPropagates(t *testing.T) {
	r := NewKeysRefreshRepository(nil, nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRows: &oneRowThenScanErrRows{scanErr: wantErr}}

	got, err := r.ListPending(ctxWithFakeTx(tx), 10)
	require.ErrorIs(t, err, wantErr)
	assert.Nil(t, got)
}

func TestKeysRefreshRepository_MarkIntent_ScanErrorPropagates(t *testing.T) {
	r := NewKeysRefreshRepository(nil, nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	_, err := r.MarkIntent(ctxWithFakeTx(tx), uuid.New(), time.Minute)
	require.ErrorIs(t, err, wantErr)
}

func TestKeysRefreshRepository_PendingStats_ScanErrorPropagates(t *testing.T) {
	r := NewKeysRefreshRepository(nil, nil)
	wantErr := errors.New("scan boom")
	tx := &fakeRepoTx{queryRowResults: []pgx.Row{errRow{err: wantErr}}}

	count, age, err := r.PendingStats(ctxWithFakeTx(tx))
	require.ErrorIs(t, err, wantErr)
	assert.Zero(t, count)
	assert.Zero(t, age)
}

// statsRow scans a fixed (count, age seconds) pair.
type statsRow struct {
	count int
	age   float64
}

func (r statsRow) Scan(dest ...any) error {
	*dest[0].(*int) = r.count
	*dest[1].(*float64) = r.age
	return nil
}

func TestKeysRefreshRepository_PendingStats_ConvertsAndClampsAge(t *testing.T) {
	r := NewKeysRefreshRepository(nil, nil)

	tx := &fakeRepoTx{queryRowResults: []pgx.Row{statsRow{count: 3, age: 90.5}}}
	count, age, err := r.PendingStats(ctxWithFakeTx(tx))
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.Equal(t, 90500*time.Millisecond, age)

	// A marker newer than the reading transaction's now() must not report a
	// negative age.
	tx = &fakeRepoTx{queryRowResults: []pgx.Row{statsRow{count: 1, age: -0.002}}}
	_, age, err = r.PendingStats(ctxWithFakeTx(tx))
	require.NoError(t, err)
	assert.Zero(t, age)
}
