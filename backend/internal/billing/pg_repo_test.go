package billing

// pg_repo_test.go exercises PGOrderRepo against pgxmock (CONTRACT §17.3).
// Same-package so it can reference the unexported sql* constants and the
// nullInt64 / nullStr / nullTimePtr helpers; QueryMatcherEqual because the SQL
// text contains regex metacharacters. Each method gets a happy path (RETURNING
// id / scan round-trip incl. Items) plus the key error / not-found / nil-pool
// cases. fixedNow / paidAt are defined in service_test.go (same package).

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

// orderColumns returns the 16 columns scanOrder reads, in scan order. This is
// the load-bearing contract between the SELECT/RETURNING clauses and scanOrder.
func orderColumns() []string {
	return []string{"id", "tenant_id", "store_id", "member_id", "order_no", "channel", "status", "total_cents", "paid_cents", "discount_cents", "payment_method", "operator_id", "paid_at", "payment_ref", "created_at", "updated_at"}
}

// itemColumns returns the 6 columns scanOrderItem reads, in scan order.
func itemColumns() []string {
	return []string{"product_id", "name_snapshot", "unit", "qty", "price_cents", "cost_cents"}
}

func TestPGOrderRepo_CreateAndGet(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	in := &Order{
		TenantID: 100, StoreID: 10, MemberID: 7, OrderNo: "ZG1", Channel: "in_store",
		Status: orderStatusPaid, TotalCents: 800, PaidCents: 800, DiscountCents: 0,
		PaymentMethod: "cash", OperatorID: 9, PaidAt: &fixedNow,
		CreatedAt: fixedNow, UpdatedAt: fixedNow,
		Items: []OrderItem{{ProductID: 1, NameSnapshot: "可乐", Unit: "瓶", Qty: 2, PriceCents: 300, CostCents: 200}},
	}

	// Create runs inside a transaction (pgxmock satisfies txBeginner): the order
	// INSERT and the item INSERT are wrapped in Begin/Commit so a failure on any
	// item rolls back the half-written order.
	mock.ExpectBegin()
	// INSERT order RETURNING id, created_at, updated_at — args mirror Create.
	mock.ExpectQuery(sqlOrderCreate).
		WithArgs(int64(100), int64(10), nullInt64(7), "ZG1", "in_store", orderStatusPaid,
			int64(800), int64(800), int64(0), nullStr("cash"), nullInt64(9),
			nullTimePtr(&fixedNow), nullStr(""), fixedNow, fixedNow).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(int64(42), fixedNow, fixedNow))
	// INSERT the one item carrying the allocated order id.
	mock.ExpectExec(sqlOrderItemCreate).
		WithArgs(int64(100), int64(42), int64(1), "可乐", "瓶", int64(2), int64(300), int64(200)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	created, err := repo.Create(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, int64(42), created.ID)
	require.Len(t, created.Items, 1)

	// GetByID: SELECT order then SELECT items.
	mock.ExpectQuery(sqlOrderGetByID).
		WithArgs(int64(100), int64(42)).
		WillReturnRows(pgxmock.NewRows(orderColumns()).AddRow(
			int64(42), int64(100), int64(10), int64(7), "ZG1", "in_store", orderStatusPaid,
			int64(800), int64(800), int64(0), "cash", int64(9), fixedNow, nil, fixedNow, fixedNow,
		))
	mock.ExpectQuery(sqlOrderItemsByOrder).
		WithArgs(int64(100), int64(42)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(1), "可乐", "瓶", int64(2), int64(300), int64(200)))

	got, err := repo.GetByID(context.Background(), 100, 42)
	require.NoError(t, err)
	require.Equal(t, int64(7), got.MemberID)
	require.Equal(t, int64(9), got.OperatorID)
	require.Equal(t, "cash", got.PaymentMethod)
	require.Equal(t, "", got.PaymentRef) // cash order -> NULL -> ""
	require.NotNil(t, got.PaidAt)
	require.Len(t, got.Items, 1)
	require.Equal(t, "可乐", got.Items[0].NameSnapshot)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGOrderRepo_Create_ItemFailureRollback is the load-bearing atomicity case
// for Create: the order INSERT succeeds but a later item INSERT fails, so the
// whole transaction must Rollback — no half-written order may be committed.
func TestPGOrderRepo_Create_ItemFailureRollback(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	in := &Order{
		TenantID: 100, StoreID: 10, MemberID: 7, OrderNo: "ZG2", Channel: "in_store",
		Status: orderStatusPaid, TotalCents: 800, PaidCents: 800,
		PaymentMethod: "cash", PaidAt: &fixedNow,
		CreatedAt: fixedNow, UpdatedAt: fixedNow,
		Items: []OrderItem{
			{ProductID: 1, NameSnapshot: "可乐", Unit: "瓶", Qty: 2, PriceCents: 300, CostCents: 200},
			{ProductID: 2, NameSnapshot: "薯片", Unit: "包", Qty: 1, PriceCents: 200, CostCents: 100},
		},
	}

	mock.ExpectBegin()
	// Order INSERT succeeds, allocating id 43.
	mock.ExpectQuery(sqlOrderCreate).
		WithArgs(int64(100), int64(10), nullInt64(7), "ZG2", "in_store", orderStatusPaid,
			int64(800), int64(800), int64(0), nullStr("cash"), nullInt64(0),
			nullTimePtr(&fixedNow), nullStr(""), fixedNow, fixedNow).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(int64(43), fixedNow, fixedNow))
	// First item INSERT succeeds.
	mock.ExpectExec(sqlOrderItemCreate).
		WithArgs(int64(100), int64(43), int64(1), "可乐", "瓶", int64(2), int64(300), int64(200)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Second item INSERT fails -> the tx must roll back (no Commit).
	mock.ExpectExec(sqlOrderItemCreate).
		WithArgs(int64(100), int64(43), int64(2), "薯片", "包", int64(1), int64(200), int64(100)).
		WillReturnError(errors.New("boom: item insert failed"))
	mock.ExpectRollback()

	_, err := repo.Create(context.Background(), in)
	require.Error(t, err)
	require.Contains(t, err.Error(), "order item create")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGOrderRepo_Create_OrderInsertFailureRollback covers the order INSERT
// itself failing inside the transaction: Begin then Rollback, no item INSERTs.
func TestPGOrderRepo_Create_OrderInsertFailureRollback(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	in := &Order{
		TenantID: 100, StoreID: 10, OrderNo: "ZG3", Channel: "in_store",
		Status: orderStatusPaid, TotalCents: 300, PaidCents: 300,
		CreatedAt: fixedNow, UpdatedAt: fixedNow,
		Items: []OrderItem{{ProductID: 1, NameSnapshot: "可乐", Unit: "瓶", Qty: 1, PriceCents: 300, CostCents: 200}},
	}

	mock.ExpectBegin()
	mock.ExpectQuery(sqlOrderCreate).
		WithArgs(int64(100), int64(10), nullInt64(0), "ZG3", "in_store", orderStatusPaid,
			int64(300), int64(300), int64(0), nullStr(""), nullInt64(0),
			nullTimePtr(nil), nullStr(""), fixedNow, fixedNow).
		WillReturnError(errors.New("boom: order insert failed"))
	mock.ExpectRollback()

	_, err := repo.Create(context.Background(), in)
	require.Error(t, err)
	require.Contains(t, err.Error(), "order create")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_GetByIDNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)
	mock.ExpectQuery(sqlOrderGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetByID(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_ListByDate(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	// Bounds must match ListByDate: natural day in day's own location.
	y, m, d := fixedNow.Date()
	loc := fixedNow.Location()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	next := start.AddDate(0, 0, 1)

	mock.ExpectQuery(sqlOrdersByDateStore).
		WithArgs(int64(100), int64(10), start, next).
		WillReturnRows(pgxmock.NewRows(orderColumns()).
			AddRow(int64(1), int64(100), int64(10), int64(7), "ZGa", "in_store", orderStatusPaid, int64(100), int64(100), int64(0), "cash", int64(0), fixedNow, nil, fixedNow, fixedNow).
			AddRow(int64(2), int64(100), int64(10), nil, "ZGb", "wxshop", orderStatusPaid, int64(200), int64(200), int64(0), nil, int64(0), fixedNow, nil, fixedNow, fixedNow))
	// Items per order (walk-in order 2 still loads its items).
	mock.ExpectQuery(sqlOrderItemsByOrder).WithArgs(int64(100), int64(1)).WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(1), "品A", "个", int64(1), int64(100), int64(40)))
	mock.ExpectQuery(sqlOrderItemsByOrder).WithArgs(int64(100), int64(2)).WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(2), "品B", "个", int64(1), int64(200), int64(80)))

	got, err := repo.ListByDate(context.Background(), 100, 10, fixedNow)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(7), got[0].MemberID)
	require.Equal(t, int64(0), got[1].MemberID) // walk-in NULL -> 0
	require.Len(t, got[1].Items, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_ListByMember(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	// newest-first (created_at DESC, id DESC): order 2 then order 1.
	mock.ExpectQuery(sqlOrdersByMember).
		WithArgs(int64(100), int64(7)).
		WillReturnRows(pgxmock.NewRows(orderColumns()).
			AddRow(int64(2), int64(100), int64(10), int64(7), "ZGb", "in_store", orderStatusPaid, int64(300), int64(300), int64(0), "cash", int64(0), fixedNow, nil, fixedNow.Add(time.Minute), fixedNow.Add(time.Minute)).
			AddRow(int64(1), int64(100), int64(10), int64(7), "ZGa", "in_store", orderStatusPaid, int64(100), int64(100), int64(0), "cash", int64(0), fixedNow, nil, fixedNow, fixedNow))
	mock.ExpectQuery(sqlOrderItemsByOrder).WithArgs(int64(100), int64(2)).WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(2), "品B", "个", int64(1), int64(300), int64(150)))
	mock.ExpectQuery(sqlOrderItemsByOrder).WithArgs(int64(100), int64(1)).WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(1), "品A", "个", int64(1), int64(100), int64(50)))

	got, err := repo.ListByMember(context.Background(), 100, 7)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(2), got[0].ID) // DESC
	require.Equal(t, int64(1), got[1].ID)
	require.Len(t, got[0].Items, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_OrderTotalByMembers(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)

	// member_id = ANY($2) receives a Go []int64 directly; pgxmock matches the
	// slice value. Single SUM column scanned into &int64.
	mock.ExpectQuery(sqlOrderTotalByMembers).
		WithArgs(int64(100), []int64{1, 2, 3}, since, until).
		WillReturnRows(pgxmock.NewRows([]string{"sum"}).AddRow(int64(1750)))

	got, err := repo.OrderTotalByMembers(context.Background(), 100, []int64{1, 2, 3}, since, until)
	require.NoError(t, err)
	require.Equal(t, int64(1750), got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_OrderTotalByMembers_EmptyAndReversed(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)

	// Empty memberIDs short-circuits before any SQL (no expectation set).
	got, err := repo.OrderTotalByMembers(context.Background(), 100, nil, since, until)
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
	// Reversed range short-circuits too.
	got, err = repo.OrderTotalByMembers(context.Background(), 100, []int64{1}, until, since)
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_RepurchaseMembers(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)
	since := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)

	mock.ExpectQuery(sqlRepurchaseMembers).
		WithArgs(int64(100), []int64{1, 2, 3}, since, until).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))

	got, err := repo.RepurchaseMembers(context.Background(), 100, []int64{1, 2, 3}, since, until)
	require.NoError(t, err)
	require.Equal(t, 2, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_UpdateStatus_PendingToPaid(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	// Step 1: load current (pending).
	mock.ExpectQuery(sqlOrderGetByID).
		WithArgs(int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(orderColumns()).AddRow(
			int64(5), int64(100), int64(10), int64(7), "ZG5", "wxshop", orderStatusPending, int64(800), int64(0), int64(0), nil, int64(9), nil, nil, fixedNow, fixedNow,
		))
	// Step 2: load items.
	mock.ExpectQuery(sqlOrderItemsByOrder).
		WithArgs(int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(1), "可乐", "瓶", int64(2), int64(400), int64(200)))
	// Step 3: conditional pending->paid UPDATE RETURNING the paid row.
	mock.ExpectQuery(sqlOrderMarkPaid).
		WithArgs(int64(800), nullTimePtr(&paidAt), nullStr("OTN-1"), int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(orderColumns()).AddRow(
			int64(5), int64(100), int64(10), int64(7), "ZG5", "wxshop", orderStatusPaid, int64(800), int64(800), int64(0), nil, int64(9), paidAt, "OTN-1", fixedNow, paidAt,
		))

	upd, err := repo.UpdateStatus(context.Background(), 100, 5, orderStatusPaid, 800, &paidAt, "OTN-1")
	require.NoError(t, err)
	require.Equal(t, orderStatusPaid, upd.Status)
	require.Equal(t, int64(800), upd.PaidCents)
	require.Equal(t, "OTN-1", upd.PaymentRef)
	require.NotNil(t, upd.PaidAt)
	require.Len(t, upd.Items, 1)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_UpdateStatus_Idempotent(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	// Load returns already-paid -> no MarkPaid UPDATE issued.
	mock.ExpectQuery(sqlOrderGetByID).
		WithArgs(int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(orderColumns()).AddRow(
			int64(5), int64(100), int64(10), int64(7), "ZG5", "wxshop", orderStatusPaid, int64(800), int64(800), int64(0), nil, int64(9), fixedNow, "OTN-A", fixedNow, fixedNow,
		))
	mock.ExpectQuery(sqlOrderItemsByOrder).
		WithArgs(int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(1), "可乐", "瓶", int64(2), int64(400), int64(200)))

	// Duplicate notify with a different ref/time: idempotent, ref not overwritten.
	upd, err := repo.UpdateStatus(context.Background(), 100, 5, orderStatusPaid, 999, &paidAt, "OTN-B")
	require.NoError(t, err)
	require.Equal(t, orderStatusPaid, upd.Status)
	require.Equal(t, "OTN-A", upd.PaymentRef) // original preserved
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_UpdateStatus_Conflict(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	mock.ExpectQuery(sqlOrderGetByID).
		WithArgs(int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(orderColumns()).AddRow(
			int64(5), int64(100), int64(10), int64(7), "ZG5", "wxshop", "refunded", int64(800), int64(800), int64(0), nil, int64(9), fixedNow, "OTN-1", fixedNow, fixedNow,
		))
	mock.ExpectQuery(sqlOrderItemsByOrder).
		WithArgs(int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(1), "可乐", "瓶", int64(2), int64(400), int64(200)))

	_, err := repo.UpdateStatus(context.Background(), 100, 5, orderStatusPaid, 800, &paidAt, "OTN-1")
	require.ErrorIs(t, err, ErrStatusConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_UpdateStatus_NonPaidTarget(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)

	// Current is paid with payment fields set; target voided must not clear them.
	mock.ExpectQuery(sqlOrderGetByID).
		WithArgs(int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(orderColumns()).AddRow(
			int64(5), int64(100), int64(10), int64(7), "ZG5", "wxshop", orderStatusPaid, int64(800), int64(800), int64(0), nil, int64(9), fixedNow, "OTN-1", fixedNow, fixedNow,
		))
	mock.ExpectQuery(sqlOrderItemsByOrder).
		WithArgs(int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(itemColumns()).AddRow(int64(1), "可乐", "瓶", int64(2), int64(400), int64(200)))
	// Plain status UPDATE RETURNING the row (payment fields unchanged).
	mock.ExpectQuery(sqlOrderUpdateStatus).
		WithArgs("voided", int64(100), int64(5)).
		WillReturnRows(pgxmock.NewRows(orderColumns()).AddRow(
			int64(5), int64(100), int64(10), int64(7), "ZG5", "wxshop", "voided", int64(800), int64(800), int64(0), nil, int64(9), fixedNow, "OTN-1", fixedNow, fixedNow,
		))

	upd, err := repo.UpdateStatus(context.Background(), 100, 5, "voided", 0, nil, "")
	require.NoError(t, err)
	require.Equal(t, "voided", upd.Status)
	require.Equal(t, int64(800), upd.PaidCents) // untouched
	require.Equal(t, "OTN-1", upd.PaymentRef)   // untouched
	require.NotNil(t, upd.PaidAt)               // untouched
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGOrderRepo_UpdateStatus_NotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGOrderRepo(mock)
	mock.ExpectQuery(sqlOrderGetByID).
		WithArgs(int64(100), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateStatus(context.Background(), 100, 99, orderStatusPaid, 800, &paidAt, "OTN")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	repo := NewPGOrderRepo(nil)

	_, err := repo.GetByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.Create(context.Background(), &Order{TenantID: 1})
	require.ErrorIs(t, err, ErrNotFound)

	out, err := repo.ListByDate(context.Background(), 1, 0, fixedNow)
	require.NoError(t, err)
	require.Empty(t, out)

	mb, err := repo.ListByMember(context.Background(), 1, 7)
	require.NoError(t, err)
	require.Empty(t, mb)

	tot, err := repo.OrderTotalByMembers(context.Background(), 1, []int64{1}, fixedNow, fixedNow)
	require.NoError(t, err)
	require.Equal(t, int64(0), tot)

	rep, err := repo.RepurchaseMembers(context.Background(), 1, []int64{1}, fixedNow, fixedNow)
	require.NoError(t, err)
	require.Equal(t, 0, rep)

	_, err = repo.UpdateStatus(context.Background(), 1, 1, orderStatusPaid, 1, &fixedNow, "x")
	require.ErrorIs(t, err, ErrNotFound)
}
