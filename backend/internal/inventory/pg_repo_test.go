package inventory

// pg_repo_test.go exercises the pgx-backed PGBatchRepo against pgxmock (CONTRACT
// §17.3). The pool uses QueryMatcherEqual because the SQL text contains regex
// metacharacters; tests are same-package so they reference the unexported sql*
// constants. Column-name helpers mirror the scanXxx Scan order exactly — that
// ordering is what makes pgxmock's row mapping work. ConsumeFIFOAtomic is the
// load-bearing case: a real SELECT...FOR UPDATE txn that touches multiple lots,
// commits on full coverage, and rolls back (returning ErrInsufficientStock)
// otherwise.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// fixedTime is the deterministic clock anchor for all PG repo tests.
var fixedTime = time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// batchColumns mirrors scanBatch's Scan order (inventory_batches).
func batchColumns() []string {
	return []string{"id", "tenant_id", "store_id", "product_id", "quantity", "in_price_cents", "expiry_date", "expiry_warn_date", "status", "created_at", "updated_at"}
}

// movementColumns mirrors scanMovement's Scan order (inventory_movements).
func movementColumns() []string {
	return []string{"id", "tenant_id", "store_id", "product_id", "batch_id", "movement_type", "quantity", "ref_order_id", "operator_id", "note", "created_at"}
}

// oneBatchRow builds a single-row batches result for the given field values.
func oneBatchRow(id, tenant, store, product, qty, price int64, status string, created time.Time) *pgxmock.Rows {
	return pgxmock.NewRows(batchColumns()).AddRow(
		id, tenant, store, product, qty, price, nil, nil, status, created, created,
	)
}

func TestPGBatchRepo_CreateAndGet(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)

	mock.ExpectQuery(sqlBatchCreate).
		WithArgs(int64(1), int64(10), int64(77), int64(100), int64(5000), fixedTime, nil, BatchStatusActive).
		WillReturnRows(pgxmock.NewRows(batchColumns()).AddRow(
			int64(7001), int64(1), int64(10), int64(77), int64(100), int64(5000), fixedTime, nil, BatchStatusActive, fixedTime, fixedTime,
		))
	created, err := repo.CreateBatch(context.Background(), &Batch{
		TenantID: 1, StoreID: 10, ProductID: 77, Quantity: 100, InPriceCents: 5000,
		ExpiryDate: &fixedTime, Status: BatchStatusActive,
	})
	require.NoError(t, err)
	require.Equal(t, int64(7001), created.ID)
	require.Equal(t, int64(100), created.Quantity)
	require.NotNil(t, created.ExpiryDate)
	require.Equal(t, fixedTime, *created.ExpiryDate)
	require.Nil(t, created.ExpiryWarnDate) // NULL -> nil pointer

	mock.ExpectQuery(sqlBatchGetByID).
		WithArgs(int64(1), int64(7001)).
		WillReturnRows(pgxmock.NewRows(batchColumns()).AddRow(
			int64(7001), int64(1), int64(10), int64(77), int64(100), int64(5000), fixedTime, nil, BatchStatusActive, fixedTime, fixedTime,
		))
	got, err := repo.GetBatch(context.Background(), 1, 7001)
	require.NoError(t, err)
	require.Equal(t, int64(7001), got.ID)
	require.Equal(t, int64(100), got.Quantity)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_GetNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// ErrNoRows (absent OR cross-tenant — the WHERE tenant_id makes them
	// indistinguishable) maps to the domain ErrNotFound.
	mock.ExpectQuery(sqlBatchGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetBatch(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_Update(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// quantity=0, status=cleared, addressed by tenant+id; RETURNING yields the
	// refreshed row (updated_at here is whatever the mock returns).
	mock.ExpectQuery(sqlBatchUpdate).
		WithArgs(int64(0), int64(5000), nil, nil, BatchStatusCleared, int64(1), int64(7001)).
		WillReturnRows(pgxmock.NewRows(batchColumns()).AddRow(
			int64(7001), int64(1), int64(10), int64(77), int64(0), int64(5000), nil, nil, BatchStatusCleared, fixedTime, fixedTime,
		))
	updated, err := repo.UpdateBatch(context.Background(), &Batch{
		ID: 7001, TenantID: 1, StoreID: 10, ProductID: 77, Quantity: 0, InPriceCents: 5000, Status: BatchStatusCleared,
	})
	require.NoError(t, err)
	require.Equal(t, int64(0), updated.Quantity)
	require.Equal(t, BatchStatusCleared, updated.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_UpdateNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// RETURNING yields no row -> ErrNotFound (absent or cross-tenant).
	mock.ExpectQuery(sqlBatchUpdate).
		WithArgs(int64(5), int64(5000), nil, nil, BatchStatusActive, int64(2), int64(8888)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateBatch(context.Background(), &Batch{
		ID: 8888, TenantID: 2, Quantity: 5, InPriceCents: 5000, Status: BatchStatusActive,
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListActiveBatchesFIFO(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// Two active lots, oldest-first is the DB's job (ORDER BY created_at, id);
	// the repo only scans. Args carry the tenant/store/product scope.
	mock.ExpectQuery(sqlBatchListActiveFIFO).
		WithArgs(int64(1), int64(10), int64(77)).
		WillReturnRows(pgxmock.NewRows(batchColumns()).
			AddRow(int64(7001), int64(1), int64(10), int64(77), int64(3), int64(5000), nil, nil, BatchStatusActive, fixedTime, fixedTime).
			AddRow(int64(7002), int64(1), int64(10), int64(77), int64(5), int64(5200), nil, nil, BatchStatusActive, fixedTime, fixedTime))
	got, err := repo.ListActiveBatchesFIFO(context.Background(), 1, 10, 77)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(7001), got[0].ID)
	require.Equal(t, int64(7002), got[1].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListNearingBatches(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// Pin the shared repo clock so the deadline arg (now()+withinDays) is
	// deterministic — the impl reads the same now() the test does.
	restore := setNowForTest(func() time.Time { return fixedTime })
	defer restore()
	withinDays := 3
	deadline := fixedTime.AddDate(0, 0, withinDays)

	mock.ExpectQuery(sqlBatchListNearing).
		WithArgs(int64(1), int64(10), deadline).
		WillReturnRows(pgxmock.NewRows(batchColumns()).
			AddRow(int64(7001), int64(1), int64(10), int64(77), int64(3), int64(5000), fixedTime, nil, BatchStatusActive, fixedTime, fixedTime))
	got, err := repo.ListNearingBatches(context.Background(), 1, 10, withinDays)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(7001), got[0].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_AppendMovement(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// movementInsertArgs: tenant, store, product, batch_id(0->NULL? no, 7001),
	// type, qty, ref_order(0->nil), operator(42), note, created_at.
	mock.ExpectQuery(sqlMovementInsert).
		WithArgs(int64(1), int64(10), int64(77), int64(7001), MovementTypePurchaseIn, int64(100), nil, int64(42), "入库", fixedTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(9001)))
	got, err := repo.AppendMovement(context.Background(), &Movement{
		TenantID: 1, StoreID: 10, ProductID: 77, BatchID: 7001, Type: MovementTypePurchaseIn,
		Quantity: 100, RefOrderID: 0, OperatorID: 42, Note: "入库", CreatedAt: fixedTime,
	})
	require.NoError(t, err)
	require.Equal(t, int64(9001), got.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListMovements_WithDayFilter(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// Day filter computes a natural-day window in Day's own location.
	day := time.Date(2026, 7, 20, 15, 30, 0, 0, time.UTC)
	y, mo, d := day.Date()
	start := time.Date(y, mo, d, 0, 0, 0, 0, day.Location())
	end := start.AddDate(0, 0, 1)

	mock.ExpectQuery(sqlMovementList).
		WithArgs(int64(1), int64(10), int64(77), MovementTypeSaleOut, start, end, 50, 0).
		WillReturnRows(pgxmock.NewRows(movementColumns()).
			AddRow(int64(9001), int64(1), int64(10), int64(77), int64(7001), MovementTypeSaleOut, int64(-3), int64(500), int64(42), nil, fixedTime))
	got, err := repo.ListMovements(context.Background(), 1, MovementFilter{
		StoreID: 10, ProductID: 77, Type: MovementTypeSaleOut, Day: &day,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(-3), got[0].Quantity)
	require.Equal(t, int64(500), got[0].RefOrderID)
	require.Equal(t, "", got[0].Note) // NULL note -> ""
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListMovements_NoFilter(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// No filter: store/product pass 0, type passes "", day window passes nil/nil
	// so the ($N=0 OR ...) and IS NULL short-circuit branches match every row.
	mock.ExpectQuery(sqlMovementList).
		WithArgs(int64(1), int64(0), int64(0), "", nil, nil, 50, 0).
		WillReturnRows(pgxmock.NewRows(movementColumns()))
	got, err := repo.ListMovements(context.Background(), 1, MovementFilter{})
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGBatchRepo_ConsumeFIFOAtomic_MultiBatch is the FIFO load-bearing case:
// qty=4 across two lots (3 then 1), the first drained to cleared, committed in
// one transaction with one sale_out movement per touched lot.
func TestPGBatchRepo_ConsumeFIFOAtomic_MultiBatch(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)

	t1 := fixedTime
	t2 := fixedTime.Add(time.Hour)
	commitTime := fixedTime.Add(2 * time.Hour)

	mock.ExpectBegin()
	// Lock active lots oldest-first.
	mock.ExpectQuery(sqlBatchLockFIFO).
		WithArgs(int64(1), int64(10), int64(77)).
		WillReturnRows(pgxmock.NewRows(batchColumns()).
			AddRow(int64(7001), int64(1), int64(10), int64(77), int64(3), int64(5000), nil, nil, BatchStatusActive, t1, t1).
			AddRow(int64(7002), int64(1), int64(10), int64(77), int64(5), int64(5200), nil, nil, BatchStatusActive, t2, t2))
	// Lot 1: deduct 3 -> 0, cleared.
	mock.ExpectQuery(sqlBatchConsumeUpdate).
		WithArgs(int64(0), BatchStatusCleared, int64(1), int64(7001)).
		WillReturnRows(oneBatchRow(7001, 1, 10, 77, 0, 5000, BatchStatusCleared, commitTime))
	mock.ExpectQuery(sqlMovementInsert).
		WithArgs(int64(1), int64(10), int64(77), int64(7001), MovementTypeSaleOut, int64(-3), int64(500), nil, "", commitTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(9001)))
	// Lot 2: deduct 1 -> 4, stays active.
	mock.ExpectQuery(sqlBatchConsumeUpdate).
		WithArgs(int64(4), BatchStatusActive, int64(1), int64(7002)).
		WillReturnRows(oneBatchRow(7002, 1, 10, 77, 4, 5200, BatchStatusActive, commitTime))
	mock.ExpectQuery(sqlMovementInsert).
		WithArgs(int64(1), int64(10), int64(77), int64(7002), MovementTypeSaleOut, int64(-1), int64(500), nil, "", commitTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(9002)))
	mock.ExpectCommit()

	mk := func(batchID, deducted int64) *Movement {
		return &Movement{BatchID: batchID, Quantity: -deducted, RefOrderID: 500, CreatedAt: commitTime}
	}
	updated, err := repo.ConsumeFIFOAtomic(context.Background(), 1, 10, 77, 4, mk)
	require.NoError(t, err)
	require.Len(t, updated, 2)
	require.Equal(t, int64(0), updated[0].Quantity)
	require.Equal(t, BatchStatusCleared, updated[0].Status)
	require.Equal(t, int64(4), updated[1].Quantity)
	require.Equal(t, BatchStatusActive, updated[1].Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGBatchRepo_ConsumeFIFOAtomic_Insufficient verifies the dry-run guard:
// when coverage is short the txn rolls back and returns ErrInsufficientStock
// having written nothing (no UPDATE, no movement, no COMMIT).
func TestPGBatchRepo_ConsumeFIFOAtomic_Insufficient(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)

	mock.ExpectBegin()
	mock.ExpectQuery(sqlBatchLockFIFO).
		WithArgs(int64(1), int64(10), int64(77)).
		WillReturnRows(oneBatchRow(7001, 1, 10, 77, 2, 5000, BatchStatusActive, fixedTime))
	mock.ExpectRollback()

	mk := func(batchID, deducted int64) *Movement {
		return &Movement{BatchID: batchID, Quantity: -deducted, RefOrderID: 500, CreatedAt: fixedTime}
	}
	updated, err := repo.ConsumeFIFOAtomic(context.Background(), 1, 10, 77, 5, mk) // need 5, have 2
	require.Nil(t, updated)
	require.ErrorIs(t, err, ErrInsufficientStock)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_EarliestExpiryByProduct(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// MIN(expiry_date) GROUP BY product_id collapses each product to its soonest
	// active lot; storeID=0 spans all of the tenant's stores via the ($2=0 OR ...)
	// idiom. Args carry the tenant + store scope.
	exp1 := fixedTime.AddDate(0, 0, 2)
	exp2 := fixedTime.AddDate(0, 0, 20)
	mock.ExpectQuery(sqlBatchEarliestExpiryByProduct).
		WithArgs(int64(1), int64(0)).
		WillReturnRows(pgxmock.NewRows([]string{"product_id", "min"}).
			AddRow(int64(77), exp1).
			AddRow(int64(88), exp2))
	got, err := repo.EarliestExpiryByProduct(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.True(t, got[77].Equal(exp1))
	require.True(t, got[88].Equal(exp2))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_EarliestExpiryByProduct_NilPool(t *testing.T) {
	// A nil pool degrades to an empty map (dev wires the memory repo).
	repo := NewPGBatchRepo(nil)
	got, err := repo.EarliestExpiryByProduct(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestPGBatchRepo_NilPoolIsSafe(t *testing.T) {
	// A nil repo / nil pool degrades to ErrNotFound (single / write) or an empty
	// slice (list) rather than panicking. ConsumeFIFOAtomic bails before any tx.
	repo := NewPGBatchRepo(nil)

	_, err := repo.GetBatch(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.CreateBatch(context.Background(), &Batch{TenantID: 1})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.UpdateBatch(context.Background(), &Batch{ID: 1, TenantID: 1})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.AppendMovement(context.Background(), &Movement{TenantID: 1})
	require.ErrorIs(t, err, ErrNotFound)

	out, err := repo.ListActiveBatchesFIFO(context.Background(), 1, 10, 77)
	require.NoError(t, err)
	require.Empty(t, out)

	out2, err := repo.ListNearingBatches(context.Background(), 1, 10, 3)
	require.NoError(t, err)
	require.Empty(t, out2)

	out3, err := repo.ListMovements(context.Background(), 1, MovementFilter{})
	require.NoError(t, err)
	require.Empty(t, out3)

	_, err = repo.ConsumeFIFOAtomic(context.Background(), 1, 10, 77, 1, func(int64, int64) *Movement { return nil })
	require.ErrorIs(t, err, ErrNotFound)
}

// --- ListBatches / ListBatchesCount (BE-A, §24.2) ---

func TestPGBatchRepo_ListBatches(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// Full-status listing with store+status filters, default paging (limit 50,
	// offset 0). product_id is unset (0 -> the ($3=0 OR ...) match-all branch),
	// expiry_before is unset (nil -> the $5::date IS NULL branch).
	mock.ExpectQuery(sqlBatchList).
		WithArgs(int64(1), int64(10), int64(0), BatchStatusActive, nil, 50, 0).
		WillReturnRows(pgxmock.NewRows(batchColumns()).
			AddRow(int64(7001), int64(1), int64(10), int64(77), int64(5), int64(5000), fixedTime, nil, BatchStatusActive, fixedTime, fixedTime).
			AddRow(int64(7002), int64(1), int64(10), int64(88), int64(9), int64(5200), nil, nil, BatchStatusActive, fixedTime, fixedTime))
	got, err := repo.ListBatches(context.Background(), 1, BatchFilter{StoreID: 10, Status: BatchStatusActive})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(7001), got[0].ID)
	require.Equal(t, int64(7002), got[1].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListBatches_ExpiryBefore(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// expiry_before is dereferenced by dateArg into a time.Time arg bound to
	// $5::date; store/product/status unset. Only the soon-expiry lot matches.
	cutoff := fixedTime.AddDate(0, 0, 7)
	mock.ExpectQuery(sqlBatchList).
		WithArgs(int64(1), int64(0), int64(0), "", cutoff, 50, 0).
		WillReturnRows(pgxmock.NewRows(batchColumns()).
			AddRow(int64(7001), int64(1), int64(10), int64(77), int64(5), int64(5000), fixedTime, nil, BatchStatusActive, fixedTime, fixedTime))
	got, err := repo.ListBatches(context.Background(), 1, BatchFilter{ExpiryBefore: &cutoff})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].ExpiryDate)
	require.True(t, !got[0].ExpiryDate.After(cutoff)) // expiry_date <= cutoff
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListBatches_Empty(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// No filter at all: every sentinel arg hits its match-all branch.
	mock.ExpectQuery(sqlBatchList).
		WithArgs(int64(1), int64(0), int64(0), "", nil, 50, 0).
		WillReturnRows(pgxmock.NewRows(batchColumns()))
	got, err := repo.ListBatches(context.Background(), 1, BatchFilter{})
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListBatchesCount(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	// COUNT(*) goes through QueryRow (the frozen PGPool surface); same filter
	// dimensions as ListBatches minus ORDER/LIMIT/OFFSET.
	mock.ExpectQuery(sqlBatchListCount).
		WithArgs(int64(1), int64(10), int64(0), BatchStatusActive, nil).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(7)))
	n, err := repo.ListBatchesCount(context.Background(), 1, BatchFilter{StoreID: 10, Status: BatchStatusActive})
	require.NoError(t, err)
	require.Equal(t, int64(7), n)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListBatchesCount_WithExpiryBefore(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGBatchRepo(mock)
	cutoff := fixedTime.AddDate(0, 0, 7)
	mock.ExpectQuery(sqlBatchListCount).
		WithArgs(int64(1), int64(0), int64(0), "", cutoff).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3)))
	n, err := repo.ListBatchesCount(context.Background(), 1, BatchFilter{ExpiryBefore: &cutoff})
	require.NoError(t, err)
	require.Equal(t, int64(3), n)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBatchRepo_ListBatches_NilPool(t *testing.T) {
	// A nil pool degrades to an empty slice / 0 total (dev wires the memory repo).
	repo := NewPGBatchRepo(nil)
	got, err := repo.ListBatches(context.Background(), 1, BatchFilter{Status: BatchStatusActive})
	require.NoError(t, err)
	require.Empty(t, got)
	n, err := repo.ListBatchesCount(context.Background(), 1, BatchFilter{Status: BatchStatusActive})
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
}
