package catalog

// pg_repo_test.go exercises the pgx-backed ProductRepo against pgxmock
// (CONTRACT §17.3). The pool uses QueryMatcherEqual because the SQL text
// contains regex metacharacters; the test is same-package so it references the
// unexported sqlProduct* constants. Each repo method gets a happy path plus the
// key error path (ErrNoRows → ErrNotFound, unique-violation 23505 → ErrConflict)
// and a nil-pool safety check.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// fixedTime is the deterministic clock used everywhere a test needs a stable
// timestamp (mirrors payment/helpers_test.go).
var fixedTime = time.Date(2026, time.July, 29, 10, 0, 0, 0, time.UTC)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// productColumns returns the products column list in scanProduct order.
func productColumns() []string {
	return []string{"id", "tenant_id", "store_id", "barcode", "name", "category", "unit", "cost_cents", "price_cents", "image_url", "shelf_life_days", "tags", "status", "created_at", "updated_at"}
}

func TestPGProductRepo_Create(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductCreate).
		WithArgs(int64(1), int64(10), "6901234567890", "可乐", "饮料", "瓶",
			int64(200), int64(300), "http://img", nullInt64(7), []string{"临期", "高毛利"}, "active").
		WillReturnRows(pgxmock.NewRows(productColumns()).AddRow(
			int64(1001), int64(1), int64(10), "6901234567890", "可乐", "饮料", "瓶",
			int64(200), int64(300), "http://img", int64(7), []string{"临期", "高毛利"}, "active", fixedTime, fixedTime,
		))
	p, err := repo.Create(context.Background(), &Product{
		TenantID: 1, StoreID: 10, Barcode: "6901234567890", Name: "可乐", Category: "饮料", Unit: "瓶",
		CostCents: 200, PriceCents: 300, ImageURL: "http://img", ShelfLifeDays: 7,
		Tags: []string{"临期", "高毛利"}, Status: "active",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1001), p.ID)
	require.Equal(t, "6901234567890", p.Barcode)
	require.Equal(t, "可乐", p.Name)
	require.Equal(t, "饮料", p.Category)
	require.Equal(t, 7, p.ShelfLifeDays)
	require.Equal(t, []string{"临期", "高毛利"}, p.Tags)
	require.Equal(t, fixedTime, p.CreatedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_CreateBarcodeEmptyBecomesNil(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	// Empty barcode is sent as nil so it never trips the partial UNIQUE index.
	mock.ExpectQuery(sqlProductCreate).
		WithArgs(int64(1), int64(10), nil, "散装糖", "", "斤",
			int64(0), int64(0), "", nullInt64(0), []string(nil), "active").
		WillReturnRows(pgxmock.NewRows(productColumns()).AddRow(
			int64(1002), int64(1), int64(10), nil, "散装糖", nil, "斤",
			int64(0), int64(0), nil, nil, []string{}, "active", fixedTime, fixedTime,
		))
	p, err := repo.Create(context.Background(), &Product{
		TenantID: 1, StoreID: 10, Name: "散装糖", Unit: "斤", Status: "active",
	})
	require.NoError(t, err)
	require.Equal(t, "", p.Barcode) // NULL → ""
	require.Equal(t, 0, p.ShelfLifeDays)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_CreateConflict(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductCreate).
		WithArgs(int64(1), int64(10), "6901234567890", "可乐", "饮料", "瓶",
			int64(200), int64(300), "", nullInt64(0), []string(nil), "active").
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key"})
	_, err := repo.Create(context.Background(), &Product{
		TenantID: 1, StoreID: 10, Barcode: "6901234567890", Name: "可乐", Category: "饮料", Unit: "瓶",
		CostCents: 200, PriceCents: 300, Status: "active",
	})
	require.ErrorIs(t, err, ErrConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_Update(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductUpdate).
		WithArgs(int64(10), "6901234567890", "可乐330ml", "饮料", "瓶",
			int64(220), int64(320), "http://img2", nullInt64(7), []string{"高毛利"}, "active",
			int64(1), int64(1001)).
		WillReturnRows(pgxmock.NewRows(productColumns()).AddRow(
			int64(1001), int64(1), int64(10), "6901234567890", "可乐330ml", "饮料", "瓶",
			int64(220), int64(320), "http://img2", int64(7), []string{"高毛利"}, "active", fixedTime, fixedTime,
		))
	p, err := repo.Update(context.Background(), &Product{
		ID: 1001, TenantID: 1, StoreID: 10, Barcode: "6901234567890", Name: "可乐330ml", Category: "饮料", Unit: "瓶",
		CostCents: 220, PriceCents: 320, ImageURL: "http://img2", ShelfLifeDays: 7,
		Tags: []string{"高毛利"}, Status: "active",
	})
	require.NoError(t, err)
	require.Equal(t, "可乐330ml", p.Name)
	require.Equal(t, int64(320), p.PriceCents)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_UpdateNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductUpdate).
		WithArgs(int64(10), nil, "缺失品", "", "个",
			int64(0), int64(0), "", nullInt64(0), []string(nil), "active",
			int64(1), int64(9999)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.Update(context.Background(), &Product{
		ID: 9999, TenantID: 1, StoreID: 10, Name: "缺失品", Unit: "个", Status: "active",
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_UpdateConflict(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductUpdate).
		WithArgs(int64(10), "6901234567890", "可乐", "饮料", "瓶",
			int64(200), int64(300), "", nullInt64(0), []string(nil), "active",
			int64(1), int64(1001)).
		WillReturnError(&pgconn.PgError{Code: "23505", Message: "duplicate key"})
	_, err := repo.Update(context.Background(), &Product{
		ID: 1001, TenantID: 1, StoreID: 10, Barcode: "6901234567890", Name: "可乐", Category: "饮料", Unit: "瓶",
		CostCents: 200, PriceCents: 300, Status: "active",
	})
	require.ErrorIs(t, err, ErrConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_GetByID(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductGetByID).
		WithArgs(int64(1), int64(1001)).
		WillReturnRows(pgxmock.NewRows(productColumns()).AddRow(
			int64(1001), int64(1), int64(10), "6901234567890", "可乐", "饮料", "瓶",
			int64(200), int64(300), "http://img", int64(7), []string{"临期"}, "active", fixedTime, fixedTime,
		))
	p, err := repo.GetByID(context.Background(), 1, 1001)
	require.NoError(t, err)
	require.Equal(t, "可乐", p.Name)
	require.Equal(t, []string{"临期"}, p.Tags)
	require.Equal(t, "active", p.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_GetByIDNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetByID(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_GetByBarcode(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductGetByBarcode).
		WithArgs(int64(1), int64(10), "6901234567890").
		WillReturnRows(pgxmock.NewRows(productColumns()).AddRow(
			int64(1001), int64(1), int64(10), "6901234567890", "可乐", "饮料", "瓶",
			int64(200), int64(300), "", int64(0), []string{}, "active", fixedTime, fixedTime,
		))
	p, err := repo.GetByBarcode(context.Background(), 1, 10, "6901234567890")
	require.NoError(t, err)
	require.Equal(t, "6901234567890", p.Barcode)
	require.Equal(t, int64(10), p.StoreID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_GetByBarcodeNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	mock.ExpectQuery(sqlProductGetByBarcode).
		WithArgs(int64(1), int64(10), "0000").
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetByBarcode(context.Background(), 1, 10, "0000")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_List(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	// Filters: store_id=10, category="饮料", query="" → limit defaults to 50.
	mock.ExpectQuery(sqlProductList).
		WithArgs(int64(1), int64(10), "饮料", "", 50, 0).
		WillReturnRows(pgxmock.NewRows(productColumns()).
			AddRow(int64(1001), int64(1), int64(10), "6901234567890", "可乐", "饮料", "瓶",
				int64(200), int64(300), "", int64(0), []string{}, "active", fixedTime, fixedTime).
			AddRow(int64(1002), int64(1), int64(10), "6901111222233", "雪碧", "饮料", "瓶",
				int64(210), int64(310), "", int64(0), []string{}, "active", fixedTime, fixedTime))
	got, err := repo.List(context.Background(), 1, ProductFilter{StoreID: 10, Category: "饮料"})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "可乐", got[0].Name)
	require.Equal(t, "雪碧", got[1].Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGProductRepo_ListPaginationAndQuery(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGProductRepo(mock)
	// Explicit limit/offset + a query substring (position() in name/barcode).
	mock.ExpectQuery(sqlProductList).
		WithArgs(int64(1), int64(0), "", "可", 10, 20).
		WillReturnRows(pgxmock.NewRows(productColumns()).
			AddRow(int64(1001), int64(1), int64(10), "6901234567890", "可乐", "饮料", "瓶",
				int64(200), int64(300), "", int64(0), []string{}, "active", fixedTime, fixedTime))
	got, err := repo.List(context.Background(), 1, ProductFilter{Query: "可", Limit: 10, Offset: 20})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "可乐", got[0].Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	repo := NewPGProductRepo(nil)
	// Writes degrade to ErrNotFound.
	_, err := repo.Create(context.Background(), &Product{Name: "x", Unit: "个", Status: "active"})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = repo.Update(context.Background(), &Product{ID: 1, TenantID: 1, Name: "x", Unit: "个", Status: "active"})
	require.ErrorIs(t, err, ErrNotFound)
	// Reads degrade to ErrNotFound.
	_, err = repo.GetByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = repo.GetByBarcode(context.Background(), 1, 1, "x")
	require.ErrorIs(t, err, ErrNotFound)
	// List degrades to an empty slice, not an error.
	out, err := repo.List(context.Background(), 1, ProductFilter{})
	require.NoError(t, err)
	require.Empty(t, out)

	// Nil repo pointer is also safe (methods on nil *PGProductRepo).
	var nilRepo *PGProductRepo
	_, err = nilRepo.GetByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
	out, err = nilRepo.List(context.Background(), 1, ProductFilter{})
	require.NoError(t, err)
	require.Empty(t, out)
}
