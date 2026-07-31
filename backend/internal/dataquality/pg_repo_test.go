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

// TestPGSalesRepo_ListDailySales covers the M7-B range read against pgxmock:
// multi-row scan, tenant+store+range args (sale_date bound as YYYY-MM-DD
// strings), and the product_id/sale_date ordering the SQL declares.
func TestPGSalesRepo_ListDailySales(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	d20 := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	d22 := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(sqlListDailySales).
		WithArgs(int64(1), int64(0), "2026-07-20", "2026-07-25").
		WillReturnRows(pgxmock.NewRows([]string{"product_id", "sale_date", "qty"}).
			AddRow(int64(10), d20, int64(5)).
			AddRow(int64(10), d22, int64(2)).
			AddRow(int64(20), d20, int64(3)))

	from := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
	rows, err := repo.ListDailySales(context.Background(), 1, 0, from, to)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, int64(10), rows[0].ProductID)
	require.True(t, rows[0].Day.Equal(d20))
	require.Equal(t, int64(5), rows[0].Qty)
	require.Equal(t, int64(20), rows[2].ProductID)
	require.Equal(t, int64(3), rows[2].Qty)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGSalesRepo_ListDailySalesNoRows: an empty result set yields an empty
// (non-nil) slice + nil error (cold-start safe).
func TestPGSalesRepo_ListDailySalesNoRows(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	mock.ExpectQuery(sqlListDailySales).
		WithArgs(int64(1), int64(0), "2026-07-20", "2026-07-25").
		WillReturnRows(pgxmock.NewRows([]string{"product_id", "sale_date", "qty"}))

	rows, err := repo.ListDailySales(context.Background(), 1, 0,
		time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NotNil(t, rows)
	require.Empty(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGSalesRepo_ListDailySalesErrorWrapped: a query error is surfaced wrapped.
func TestPGSalesRepo_ListDailySalesErrorWrapped(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	boom := errors.New("boom")
	mock.ExpectQuery(sqlListDailySales).
		WithArgs(int64(1), int64(0), "2026-07-20", "2026-07-25").
		WillReturnError(boom)

	_, err := repo.ListDailySales(context.Background(), 1, 0,
		time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC))
	require.Error(t, err)
	require.ErrorIs(t, err, boom)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGSalesRepo_ListDailySalesInvertedRange: an inverted range short-circuits
// to an empty slice before any DB round-trip (no expectations set).
func TestPGSalesRepo_ListDailySalesInvertedRange(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSalesRepo(mock)
	rows, err := repo.ListDailySales(context.Background(), 1, 0,
		time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Empty(t, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGSalesRepo_ListDailySalesNilPool: a nil pool (and nil receiver) is a safe
// no-op returning an empty slice + nil error.
func TestPGSalesRepo_ListDailySalesNilPool(t *testing.T) {
	repo := NewPGSalesRepo(nil)
	rows, err := repo.ListDailySales(context.Background(), 1, 0,
		time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Empty(t, rows)

	var nilRepo *PGSalesRepo
	rows, err = nilRepo.ListDailySales(context.Background(), 1, 0,
		time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Empty(t, rows)
}

// TestPGSalesRepo_ListDailySalesUsesRangeSQL guards the tenant-isolation + range
// contract: the read MUST filter on tenant_id and bound sale_date on both ends.
func TestPGSalesRepo_ListDailySalesUsesRangeSQL(t *testing.T) {
	require.Contains(t, sqlListDailySales, "tenant_id = $1")
	require.Contains(t, sqlListDailySales, "sale_date >= $3")
	require.Contains(t, sqlListDailySales, "sale_date <= $4")
	require.Contains(t, sqlListDailySales, "($2 = 0 OR store_id = $2)")
	require.Contains(t, sqlListDailySales, "FROM daily_product_sales")
}
