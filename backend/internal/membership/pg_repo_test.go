package membership

// pg_repo_test.go exercises PGMemberRepo against pgxmock (CONTRACT §17.3),
// mirroring internal/payment/pg_repo_test.go. The pool uses QueryMatcherEqual
// because the SQL text contains regex metacharacters; tests are same-package so
// they reference the unexported sql* constants. Column helpers' order matches
// scanMember / scanMember exactly — that alignment is what makes pgxmock row
// mapping work. The two multi-table critical sections (redeemAtomic /
// rechargeAtomic) are driven through ExpectBegin / ExpectCommit /
// ExpectRollback; the load-bearing assertions are (a) balance overdraft is
// rejected with NO audit row written (rollback) and (b) the cap breach returns
// errcode.ErrBalanceOver with NO credit.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/pkg/errcode"
)

// Compile-time assertion that PGMemberRepo satisfies MemberRepo.
var _ MemberRepo = (*PGMemberRepo)(nil)

var fixedTime = time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)

// ctx is the package-level background context the pgxmock tests thread through
// the repo methods (the atomic critical sections now take a ctx first param so
// they are no longer detached from the caller). Local test functions that need
// a tenant-scoped ctx shadow this with their own `ctx :=`.
var ctx = context.Background()

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// memberColumns mirrors scanMember's Scan order exactly.
func memberColumns() []string {
	return []string{"id", "tenant_id", "store_id", "phone_hash", "wx_openid", "name", "level", "points", "total_spent_cents", "last_visit_at", "birthday", "status", "created_at", "updated_at"}
}

// accountColumns mirrors scanAccount's Scan order exactly.
func accountColumns() []string {
	return []string{"id", "tenant_id", "member_id", "balance_cents", "gift_cents", "region_cap_cents", "created_at", "updated_at"}
}

// ===== CreateMember / GetMember =====

func TestPGMemberRepo_CreateAndGet(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)

	lv := fixedTime
	mock.ExpectQuery(sqlMemberCreate).
		WithArgs(int64(1), int64(10), nullStr("ph_123"), nullStr(""), "Alice", "normal", int64(0), int64(0), nullTimePtr(&lv), nullTimePtr(nil), "active", fixedTime, fixedTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(42)))
	created, err := repo.CreateMember(context.Background(), &Member{
		TenantID: 1, StoreID: 10, PhoneHash: "ph_123", Name: "Alice",
		Level: "normal", Status: "active", LastVisitAt: &lv, CreatedAt: fixedTime, UpdatedAt: fixedTime,
	})
	require.NoError(t, err)
	require.Equal(t, int64(42), created.ID)

	mock.ExpectQuery(sqlMemberGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(memberColumns()).AddRow(
			int64(42), int64(1), int64(10), "ph_123", nil, "Alice", "normal", int64(0), int64(0), fixedTime, nil, "active", fixedTime, fixedTime,
		))
	got, err := repo.GetMember(context.Background(), 1, 42)
	require.NoError(t, err)
	require.Equal(t, "ph_123", got.PhoneHash)
	require.Equal(t, "", got.WxOpenID) // NULL → empty
	require.NotNil(t, got.LastVisitAt)
	require.Nil(t, got.Birthday) // NULL → nil
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_GetMemberNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlMemberGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetMember(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_UpdateMember(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlMemberUpdate).
		WithArgs(nullStr("ph_1"), nullStr("oABC"), "Bob", "vip", int64(500), int64(9900), nullTimePtr(nil), nullTimePtr(nil), "active", int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(memberColumns()).AddRow(
			int64(42), int64(1), int64(10), "ph_1", "oABC", "Bob", "vip", int64(500), int64(9900), nil, nil, "active", fixedTime, fixedTime,
		))
	updated, err := repo.UpdateMember(context.Background(), &Member{
		ID: 42, TenantID: 1, StoreID: 10, PhoneHash: "ph_1", WxOpenID: "oABC", Name: "Bob",
		Level: "vip", Points: 500, TotalSpentCents: 9900, Status: "active",
	})
	require.NoError(t, err)
	require.Equal(t, int64(500), updated.Points)
	require.Equal(t, "oABC", updated.WxOpenID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_UpdateMemberNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlMemberUpdate).
		WithArgs(nullStr("ph_1"), nullStr(""), "Bob", "normal", int64(0), int64(0), nullTimePtr(nil), nullTimePtr(nil), "active", int64(1), int64(999)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateMember(context.Background(), &Member{
		ID: 999, TenantID: 1, PhoneHash: "ph_1", Name: "Bob", Level: "normal", Status: "active",
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== UpsertAccount / GetAccount =====

func TestPGMemberRepo_UpsertAccount(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlAccountUpsert).
		WithArgs(int64(1), int64(42), int64(0), int64(0), nullInt64(0), fixedTime, fixedTime).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(50), int64(1), int64(42), int64(0), int64(0), nil, fixedTime, fixedTime,
		))
	acc, err := repo.UpsertAccount(context.Background(), &BalanceAccount{
		TenantID: 1, MemberID: 42, CreatedAt: fixedTime, UpdatedAt: fixedTime,
	})
	require.NoError(t, err)
	require.Equal(t, int64(50), acc.ID)
	require.Equal(t, int64(0), acc.RegionCapCents) // NULL → 0 (uncapped)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_GetAccount(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlAccountGet).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(50), int64(1), int64(42), int64(8000), int64(2000), int64(50000), fixedTime, fixedTime,
		))
	acc, err := repo.GetAccount(context.Background(), 1, 42)
	require.NoError(t, err)
	require.Equal(t, int64(8000), acc.BalanceCents)
	require.Equal(t, int64(2000), acc.GiftCents)
	require.Equal(t, int64(50000), acc.RegionCapCents)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_GetAccountNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlAccountGet).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetAccount(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== AppendPointsLog / AppendBalanceLog =====
// The public audit appends use time.Now() internally (the interface carries no
// clock), so the created_at arg is matched with pgxmock.AnyArg().

func TestPGMemberRepo_AppendPointsLog(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectExec(sqlPointsLogAppend).
		WithArgs(int64(1), int64(42), "earn", int64(10), int64(10), nullInt64(700), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	require.NoError(t, repo.AppendPointsLog(context.Background(), 1, 42, "earn", 10, 10, 700))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_AppendBalanceLog(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectExec(sqlBalanceLogAppend).
		WithArgs(int64(1), int64(42), int64(50), "consume", int64(-500), int64(7500), nullInt64(0), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	require.NoError(t, repo.AppendBalanceLog(context.Background(), 1, 42, 50, "consume", -500, 7500, 0))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== ListMembers =====

func TestPGMemberRepo_ListMembers(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	listSQL := sqlMemberListPrefix + ` ORDER BY id LIMIT $2`
	mock.ExpectQuery(listSQL).
		WithArgs(int64(1), defaultListMembersLimit).
		WillReturnRows(pgxmock.NewRows(memberColumns()).
			AddRow(int64(42), int64(1), int64(10), "ph_1", nil, "Alice", "normal", int64(0), int64(0), nil, nil, "active", fixedTime, fixedTime).
			AddRow(int64(43), int64(1), int64(10), nil, "oABC", "Bob", "vip", int64(500), int64(0), nil, nil, "active", fixedTime, fixedTime))
	got, err := repo.ListMembers(context.Background(), 1, MemberFilter{})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "Alice", got[0].Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_ListMembersEmpty(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	listSQL := sqlMemberListPrefix + ` ORDER BY id LIMIT $2`
	mock.ExpectQuery(listSQL).
		WithArgs(int64(1), defaultListMembersLimit).
		WillReturnRows(pgxmock.NewRows(memberColumns()))
	got, err := repo.ListMembers(context.Background(), 1, MemberFilter{})
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== findMemberIDByPhone / findMemberIDByOpenID =====

func TestPGMemberRepo_FindMemberIDByPhone(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlMemberIDByPhone).
		WithArgs(int64(1), "ph_123").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(42)))
	id, ok := repo.findMemberIDByPhone(ctx, 1, "ph_123")
	require.True(t, ok)
	require.Equal(t, int64(42), id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_FindMemberIDByPhoneMiss(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlMemberIDByPhone).
		WithArgs(int64(1), "missing").
		WillReturnError(pgx.ErrNoRows)
	id, ok := repo.findMemberIDByPhone(ctx, 1, "missing")
	require.False(t, ok)
	require.Equal(t, int64(0), id)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_FindMemberIDByOpenID(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlMemberIDByOpenID).
		WithArgs(int64(1), "oABC").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(43)))
	id, ok := repo.findMemberIDByOpenID(ctx, 1, "oABC")
	require.True(t, ok)
	require.Equal(t, int64(43), id)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== redeemAtomic =====

func TestPGMemberRepo_RedeemAtomic_ConsumeAndEarn(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectBegin()
	// Conditional deduct: pre-balance 1000 - consume 500 = 500.
	mock.ExpectQuery(sqlAccountDeductBalance).
		WithArgs(int64(500), int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(50), int64(1), int64(42), int64(500), int64(0), nil, fixedTime, fixedTime,
		))
	// Consume balance-flow row (negative amount, post-deduct balance, refOrderID).
	mock.ExpectExec(sqlBalanceLogAppend).
		WithArgs(int64(1), int64(42), int64(50), "consume", int64(-500), int64(500), nullInt64(700), fixedTime).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Member touch: points 0 + earn 10 = 10; last_visit_at = now.
	mock.ExpectQuery(sqlMemberRedeemTouch).
		WithArgs(int64(10), fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(memberColumns()).AddRow(
			int64(42), int64(1), int64(10), "ph_1", nil, "Alice", "normal", int64(10), int64(0), fixedTime, nil, "active", fixedTime, fixedTime,
		))
	// Earn points-flow row.
	mock.ExpectExec(sqlPointsLogAppend).
		WithArgs(int64(1), int64(42), "earn", int64(10), int64(10), nullInt64(700), fixedTime).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	out, err := repo.redeemAtomic(ctx, 1, 42, fixedTime, 500, 10, 700)
	require.NoError(t, err)
	require.NotNil(t, out.account)
	require.Equal(t, int64(500), out.account.BalanceCents)
	require.Equal(t, int64(10), out.member.Points)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_RedeemAtomic_PointsOnly(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectBegin()
	// No consume ⇒ no account deduct / balance log.
	mock.ExpectQuery(sqlMemberRedeemTouch).
		WithArgs(int64(15), fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(memberColumns()).AddRow(
			int64(42), int64(1), int64(10), "ph_1", nil, "Alice", "normal", int64(15), int64(0), fixedTime, nil, "active", fixedTime, fixedTime,
		))
	mock.ExpectExec(sqlPointsLogAppend).
		WithArgs(int64(1), int64(42), "earn", int64(15), int64(15), nullInt64(0), fixedTime).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	out, err := repo.redeemAtomic(ctx, 1, 42, fixedTime, 0, 15, 0)
	require.NoError(t, err)
	require.Nil(t, out.account) // points-only ⇒ no account in outcome
	require.Equal(t, int64(15), out.member.Points)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGMemberRepo_RedeemAtomic_InsufficientBalance is the load-bearing
// atomicity case: overdraft is rejected with ErrInsufficientBalance and NO
// audit row is written (no balance_log / points_log Exec; the tx rolls back).
func TestPGMemberRepo_RedeemAtomic_InsufficientBalance(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectBegin()
	// Conditional deduct matches 0 rows (balance < consume).
	mock.ExpectQuery(sqlAccountDeductBalance).
		WithArgs(int64(500), int64(1), int64(42)).
		WillReturnError(pgx.ErrNoRows)
	// Disambiguation: the account EXISTS ⇒ insufficient (not missing).
	mock.ExpectQuery(sqlAccountExists).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows([]string{"one"}).AddRow(int64(1)))
	mock.ExpectRollback()

	_, err := repo.redeemAtomic(ctx, 1, 42, fixedTime, 500, 10, 700)
	require.ErrorIs(t, err, ErrInsufficientBalance)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGMemberRepo_RedeemAtomic_NoAccount proves a missing account (not
// overdraft) surfaces as ErrNotFound via the disambiguation query.
func TestPGMemberRepo_RedeemAtomic_NoAccount(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectBegin()
	mock.ExpectQuery(sqlAccountDeductBalance).
		WithArgs(int64(500), int64(1), int64(42)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(sqlAccountExists).
		WithArgs(int64(1), int64(42)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := repo.redeemAtomic(ctx, 1, 42, fixedTime, 500, 10, 700)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGMemberRepo_RedeemAtomic_MemberNotFound covers the consume==0 path where
// the member touch UPDATE matches 0 rows ⇒ ErrNotFound.
func TestPGMemberRepo_RedeemAtomic_MemberNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectBegin()
	mock.ExpectQuery(sqlMemberRedeemTouch).
		WithArgs(int64(10), fixedTime, int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := repo.redeemAtomic(ctx, 1, 99, fixedTime, 0, 10, 0)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== rechargeAtomic =====

func TestPGMemberRepo_RechargeAtomic_Happy(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectBegin()
	// Load account (balance 0, no cap).
	mock.ExpectQuery(sqlAccountGet).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(50), int64(1), int64(42), int64(0), int64(0), nil, fixedTime, fixedTime,
		))
	// Credit balance +20000, gift +2000.
	mock.ExpectQuery(sqlAccountCredit).
		WithArgs(int64(20000), int64(2000), int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(50), int64(1), int64(42), int64(20000), int64(2000), nil, fixedTime, fixedTime,
		))
	mock.ExpectExec(sqlBalanceLogAppend).
		WithArgs(int64(1), int64(42), int64(50), "recharge", int64(20000), int64(20000), nullInt64(0), fixedTime).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	acc, err := repo.rechargeAtomic(ctx, 1, 42, fixedTime, 20000, 2000, 0, 7)
	require.NoError(t, err)
	require.Equal(t, int64(20000), acc.BalanceCents)
	require.Equal(t, int64(2000), acc.GiftCents)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGMemberRepo_RechargeAtomic_CapBreach proves a regional-cap breach
// returns errcode.ErrBalanceOver with NO credit and NO audit row (rollback).
func TestPGMemberRepo_RechargeAtomic_CapBreach(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectBegin()
	// Account holds 40000, cap 50000; topping up 20000 + gift 2000 ⇒ 62000 > cap.
	mock.ExpectQuery(sqlAccountGet).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(50), int64(1), int64(42), int64(40000), int64(0), int64(50000), fixedTime, fixedTime,
		))
	mock.ExpectRollback()

	_, err := repo.rechargeAtomic(ctx, 1, 42, fixedTime, 20000, 2000, 0, 7)
	require.ErrorIs(t, err, errcode.ErrBalanceOver)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_RechargeAtomic_NoAccount(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectBegin()
	mock.ExpectQuery(sqlAccountGet).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, err := repo.rechargeAtomic(ctx, 1, 99, fixedTime, 20000, 2000, 0, 7)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== ctx threading regression (the context.Background() detachment fix) =====

// ctxKey is the private key type the ctx-threading regression tests use to tag a
// request context and assert the atomic critical sections receive it.
type ctxKey struct{}

// ctxCapturePool wraps a pgxmock pool and records the ctx handed to BeginTx, so
// the tests can assert redeemAtomic / rechargeAtomic thread the CALLER's ctx
// into the transaction (the pre-fix code used a detached context.Background(),
// losing cancellation / deadline / trace).
type ctxCapturePool struct {
	pgxmock.PgxPoolIface
	lastBeginCtx context.Context
}

func (c *ctxCapturePool) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	c.lastBeginCtx = ctx
	return c.PgxPoolIface.BeginTx(ctx, opts)
}

// TestPGMemberRepo_AtomicThreadsCallerCtx is the regression test for the
// "atomic critical sections used context.Background()" bug. It proves both
// redeemAtomic and rechargeAtomic propagate the caller's ctx into BeginTx:
//   - the request-scoped value survives (trace / tenant context is preserved);
//   - the caller's cancellation is detached via context.WithoutCancel, so an
//     already-cancelled request ctx does NOT abort a money movement mid-flight
//     (the tx still commits) — the deliberate half-commit-ambiguity choice.
func TestPGMemberRepo_AtomicThreadsCallerCtx(t *testing.T) {
	mock := newMockPool(t)
	cap := &ctxCapturePool{PgxPoolIface: mock}
	repo := NewPGMemberRepo(cap)

	// A request ctx that carries a trace-style value AND is already cancelled
	// (simulating a client that disconnected after the handler started).
	base := context.WithValue(context.Background(), ctxKey{}, "trace-123")
	cctx, cancel := context.WithCancel(base)
	cancel()

	// --- redeemAtomic: value propagates, cancellation is detached (commits). ---
	mock.ExpectBegin()
	mock.ExpectQuery(sqlMemberRedeemTouch).
		WithArgs(int64(10), fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(memberColumns()).AddRow(
			int64(42), int64(1), int64(10), "ph_1", nil, "Alice", "normal", int64(10), int64(0), fixedTime, nil, "active", fixedTime, fixedTime,
		))
	mock.ExpectExec(sqlPointsLogAppend).
		WithArgs(int64(1), int64(42), "earn", int64(10), int64(10), nullInt64(0), fixedTime).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	_, err := repo.redeemAtomic(cctx, 1, 42, fixedTime, 0, 10, 0)
	require.NoError(t, err, "a cancelled request ctx must not abort the money movement (WithoutCancel)")
	require.NotNil(t, cap.lastBeginCtx, "BeginTx must have been called")
	require.Equal(t, "trace-123", cap.lastBeginCtx.Value(ctxKey{}), "the caller's trace value must reach the transaction")
	require.NoError(t, cap.lastBeginCtx.Err(), "cancellation must be detached so the deduction commits")
	require.NoError(t, mock.ExpectationsWereMet())

	// --- rechargeAtomic: same guarantees on the credit path. ---
	mock.ExpectBegin()
	mock.ExpectQuery(sqlAccountGet).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(50), int64(1), int64(42), int64(0), int64(0), nil, fixedTime, fixedTime,
		))
	mock.ExpectQuery(sqlAccountCredit).
		WithArgs(int64(20000), int64(2000), int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(accountColumns()).AddRow(
			int64(50), int64(1), int64(42), int64(20000), int64(2000), nil, fixedTime, fixedTime,
		))
	mock.ExpectExec(sqlBalanceLogAppend).
		WithArgs(int64(1), int64(42), int64(50), "recharge", int64(20000), int64(20000), nullInt64(0), fixedTime).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	_, err = repo.rechargeAtomic(cctx, 1, 42, fixedTime, 20000, 2000, 0, 7)
	require.NoError(t, err, "a cancelled request ctx must not abort the money movement (WithoutCancel)")
	require.Equal(t, "trace-123", cap.lastBeginCtx.Value(ctxKey{}), "the caller's trace value must reach the transaction")
	require.NoError(t, cap.lastBeginCtx.Err(), "cancellation must be detached so the credit commits")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPGMemberRepo_FindMemberIDThreadsCallerCtx proves the idempotency lookups
// now receive the caller's ctx (pre-fix they used a detached
// context.Background(), so a cancelled/timed-out request still issued the query
// with no trace linkage).
func TestPGMemberRepo_FindMemberIDThreadsCallerCtx(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)

	base := context.WithValue(context.Background(), ctxKey{}, "trace-xyz")

	mock.ExpectQuery(sqlMemberIDByPhone).
		WithArgs(int64(1), "ph_123").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(42)))
	id, ok := repo.findMemberIDByPhone(base, 1, "ph_123")
	require.True(t, ok)
	require.Equal(t, int64(42), id)

	mock.ExpectQuery(sqlMemberIDByOpenID).
		WithArgs(int64(1), "oABC").
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(43)))
	id, ok = repo.findMemberIDByOpenID(base, 1, "oABC")
	require.True(t, ok)
	require.Equal(t, int64(43), id)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== UpdateAccountRegionCap =====

func TestPGMemberRepo_UpdateAccountRegionCap(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectExec(sqlAccountSetRegionCap).
		WithArgs(nullInt64(int64(50000)), int64(1), int64(42)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, repo.UpdateAccountRegionCap(context.Background(), 1, 42, 50000))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_UpdateAccountRegionCapNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectExec(sqlAccountSetRegionCap).
		WithArgs(nullInt64(int64(50000)), int64(1), int64(99)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0)) // 0 rows ⇒ ErrNotFound
	require.ErrorIs(t, repo.UpdateAccountRegionCap(context.Background(), 1, 99, 50000), ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== Nil-pool safety =====

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	repo := NewPGMemberRepo(nil)

	// Reads / atomic critical sections degrade to ErrNotFound.
	_, err := repo.GetMember(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.GetAccount(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.UpsertAccount(context.Background(), &BalanceAccount{TenantID: 1, MemberID: 1})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.CreateMember(context.Background(), &Member{TenantID: 1})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.redeemAtomic(ctx, 1, 1, fixedTime, 100, 10, 0)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.rechargeAtomic(ctx, 1, 1, fixedTime, 100, 10, 0, 0)
	require.ErrorIs(t, err, ErrNotFound)

	require.ErrorIs(t, repo.UpdateAccountRegionCap(context.Background(), 1, 1, 50000), ErrNotFound)

	// Multi-row read degrades to an empty slice.
	out, err := repo.ListMembers(context.Background(), 1, MemberFilter{})
	require.NoError(t, err)
	require.Empty(t, out)

	// ListMembersPage degrades to an empty page with total 0.
	page, err := repo.ListMembersPage(context.Background(), 1, MemberFilter{})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, int64(0), page.Total)

	// ListPointsLog degrades to an empty slice.
	logs, err := repo.ListPointsLog(context.Background(), 1, 1, 50)
	require.NoError(t, err)
	require.Empty(t, logs)

	// Identity lookups degrade to (0, false).
	id, ok := repo.findMemberIDByPhone(ctx, 1, "ph")
	require.False(t, ok)
	require.Equal(t, int64(0), id)
	id, ok = repo.findMemberIDByOpenID(ctx, 1, "oABC")
	require.False(t, ok)
	require.Equal(t, int64(0), id)

	// Audit writes degrade to a no-op success (matching memory's cannot-fail
	// audit semantics, not ErrNotFound).
	require.NoError(t, repo.AppendPointsLog(context.Background(), 1, 1, "earn", 1, 1, 0))
	require.NoError(t, repo.AppendBalanceLog(context.Background(), 1, 1, 1, "consume", -1, 0, 0))

	// A typed-nil repo is likewise safe.
	var nilRepo *PGMemberRepo
	_, err = nilRepo.GetMember(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, nilRepo.AppendPointsLog(context.Background(), 1, 1, "earn", 1, 1, 0))
}

// ===== ListMembersPage (FE-MP-02 / FE-ADMIN boss list) =====

// pointsLogColumns mirrors scanPointsLog's Scan order exactly.
func pointsLogColumns() []string {
	return []string{"id", "tenant_id", "member_id", "type", "delta", "points_after", "ref_order_id", "created_at"}
}

func TestPGMemberRepo_ListMembersPage(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)

	// 1) COUNT(*) returns 2.
	countSQL := strings.Replace(sqlMemberListPrefix,
		`SELECT id, tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at FROM members`,
		`SELECT COUNT(*) FROM members`, 1)
	mock.ExpectQuery(countSQL).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(2)))

	// 2) Paged SELECT (default limit 20, offset 0) returns the 2 rows.
	pageSQL := sqlMemberListPrefix + ` ORDER BY id LIMIT $2 OFFSET $3`
	mock.ExpectQuery(pageSQL).
		WithArgs(int64(1), defaultListMembersPageLimit, 0).
		WillReturnRows(pgxmock.NewRows(memberColumns()).
			AddRow(int64(42), int64(1), int64(10), "ph_1", nil, "Alice", "normal", int64(0), int64(0), nil, nil, "active", fixedTime, fixedTime).
			AddRow(int64(43), int64(1), int64(10), nil, "oABC", "Bob", "vip", int64(500), int64(0), nil, nil, "active", fixedTime, fixedTime))

	page, err := repo.ListMembersPage(context.Background(), 1, MemberFilter{})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, int64(2), page.Total)
	require.Equal(t, "Alice", page.Items[0].Name)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_ListMembersPage_LevelFilter(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	countSQL := strings.Replace(sqlMemberListPrefix,
		`SELECT id, tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at FROM members`,
		`SELECT COUNT(*) FROM members`, 1) + ` AND level = $2`
	mock.ExpectQuery(countSQL).
		WithArgs(int64(1), "vip").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(1)))
	pageSQL := sqlMemberListPrefix + ` AND level = $2 ORDER BY id LIMIT $3 OFFSET $4`
	mock.ExpectQuery(pageSQL).
		WithArgs(int64(1), "vip", defaultListMembersPageLimit, 0).
		WillReturnRows(pgxmock.NewRows(memberColumns()).
			AddRow(int64(43), int64(1), int64(10), nil, "oABC", "Bob", "vip", int64(500), int64(0), nil, nil, "active", fixedTime, fixedTime))
	page, err := repo.ListMembersPage(context.Background(), 1, MemberFilter{Level: "vip"})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, int64(1), page.Total)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_ListMembersPage_Empty(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	countSQL := strings.Replace(sqlMemberListPrefix,
		`SELECT id, tenant_id, store_id, phone_hash, wx_openid, name, level, points, total_spent_cents, last_visit_at, birthday, status, created_at, updated_at FROM members`,
		`SELECT COUNT(*) FROM members`, 1)
	mock.ExpectQuery(countSQL).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(0)))
	page, err := repo.ListMembersPage(context.Background(), 1, MemberFilter{})
	require.NoError(t, err)
	require.Empty(t, page.Items)
	require.Equal(t, int64(0), page.Total)
	// total 0 short-circuits the paged SELECT.
	require.NoError(t, mock.ExpectationsWereMet())
}

// ===== ListPointsLog (FE-MP-02 CONTRACT §16.3(8)) =====

func TestPGMemberRepo_ListPointsLog(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlPointsLogList).
		WithArgs(int64(1), int64(42), 50).
		WillReturnRows(pgxmock.NewRows(pointsLogColumns()).
			AddRow(int64(10), int64(1), int64(42), "earn", int64(50), int64(50), int64(700), fixedTime).
			AddRow(int64(9), int64(1), int64(42), "redeem", int64(-20), int64(30), int64(701), fixedTime))
	logs, err := repo.ListPointsLog(context.Background(), 1, 42, 50)
	require.NoError(t, err)
	require.Len(t, logs, 2)
	require.Equal(t, "earn", logs[0].Type)
	require.Equal(t, int64(50), logs[0].Delta)
	require.Equal(t, int64(30), logs[1].PointsAfter)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGMemberRepo_ListPointsLog_Empty(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGMemberRepo(mock)
	mock.ExpectQuery(sqlPointsLogList).
		WithArgs(int64(1), int64(99), 50).
		WillReturnRows(pgxmock.NewRows(pointsLogColumns()))
	logs, err := repo.ListPointsLog(context.Background(), 1, 99, 50)
	require.NoError(t, err)
	require.Empty(t, logs)
	require.NoError(t, mock.ExpectationsWereMet())
}
