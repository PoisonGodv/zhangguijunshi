package dataquality

// pg_repo_test.go exercises the pgx-backed PGSalesRepo against pgxmock. The pool
// uses QueryMatcherEqual because the SQL text contains regex metacharacters; the
// test is same-package so it references the unexported sqlUpsertDailySales /
// sqlLastSaleDate constants.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

var fixedDay = time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

func TestPGSalesRepo_UpsertDailySales(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)

	// One Exec per product row; sale_date bound as a YYYY-MM-DD string.
	mock.ExpectExec(sqlUpsertDailySales).
		WithArgs(int64(1), int64(0), int64(10), "2026-07-29", int64(5), SourceSaleOut).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(sqlUpsertDailySales).
		WithArgs(int64(1), int64(0), int64(20), "2026-07-29", int64(3), SourceSaleOut).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	n, err := repo.UpsertDailySales(context.Background(), 1, 0, fixedDay, []SaleQty{{ProductID: 10, Qty: 5}, {ProductID: 20, Qty: 3}})
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGSalesRepo_UpsertErrorWrappedWithCountSoFar(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	boom := errors.New("boom")

	mock.ExpectExec(sqlUpsertDailySales).
		WithArgs(int64(1), int64(0), int64(10), "2026-07-29", int64(5), SourceSaleOut).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(sqlUpsertDailySales).
		WithArgs(int64(1), int64(0), int64(20), "2026-07-29", int64(3), SourceSaleOut).
		WillReturnError(boom)

	n, err := repo.UpsertDailySales(context.Background(), 1, 0, fixedDay, []SaleQty{{ProductID: 10, Qty: 5}, {ProductID: 20, Qty: 3}})
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
	require.Equal(t, 1, n) // one row written before the failure
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGSalesRepo_UpsertEmptyIsNoOp(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	// No expectations: an empty rows slice returns before any DB round-trip.
	n, err := repo.UpsertDailySales(context.Background(), 1, 0, fixedDay, nil)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGSalesRepo_LastSaleDate(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	mock.ExpectQuery(sqlLastSaleDate).
		WithArgs(int64(1), int64(0), int64(10)).
		WillReturnRows(pgxmock.NewRows([]string{"sale_date"}).AddRow(fixedDay))

	last, ok, err := repo.LastSaleDate(context.Background(), 1, 0, 10)
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, last.Equal(fixedDay))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGSalesRepo_LastSaleDateNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	mock.ExpectQuery(sqlLastSaleDate).
		WithArgs(int64(1), int64(0), int64(99)).
		WillReturnError(pgx.ErrNoRows)

	last, ok, err := repo.LastSaleDate(context.Background(), 1, 0, 99)
	require.NoError(t, err) // ErrNoRows → ok=false, not an error
	require.False(t, ok)
	require.True(t, last.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGSalesRepo_LastSaleDateErrorWrapped(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	boom := errors.New("boom")
	mock.ExpectQuery(sqlLastSaleDate).
		WithArgs(int64(1), int64(0), int64(10)).
		WillReturnError(boom)
	_, _, err := repo.LastSaleDate(context.Background(), 1, 0, 10)
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGSalesRepo_NilPoolIsNoOp(t *testing.T) {
	repo := NewPGSalesRepo(nil)
	n, err := repo.UpsertDailySales(context.Background(), 1, 0, fixedDay, []SaleQty{{ProductID: 10, Qty: 5}})
	require.NoError(t, err)
	require.Equal(t, 0, n)

	last, ok, err := repo.LastSaleDate(context.Background(), 1, 0, 10)
	require.NoError(t, err)
	require.False(t, ok)
	require.True(t, last.IsZero())

	// Nil receiver is also safe.
	var nilRepo *PGSalesRepo
	n, err = nilRepo.UpsertDailySales(context.Background(), 1, 0, fixedDay, []SaleQty{{ProductID: 10, Qty: 5}})
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

// TestPGSalesRepo_UpsertUsesOnConflictSQL guards the idempotency requirement:
// the upsert MUST be an INSERT ... ON CONFLICT (the unique-index upsert) so a
// re-run of the same day overwrites rather than duplicates.
func TestPGSalesRepo_UpsertUsesOnConflictSQL(t *testing.T) {
	require.Contains(t, sqlUpsertDailySales, "ON CONFLICT (tenant_id, store_id, product_id, sale_date)")
	require.Contains(t, sqlUpsertDailySales, "DO UPDATE")
	require.Contains(t, sqlUpsertDailySales, "INSERT INTO daily_product_sales")
}
