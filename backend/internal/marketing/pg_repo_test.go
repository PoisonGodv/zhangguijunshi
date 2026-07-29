package marketing

// pg_repo_test.go exercises PGCouponRepo against pgxmock (CONTRACT §17.3). The
// pool uses QueryMatcherEqual because the SQL text contains regex
// metacharacters; tests are same-package so they reference the unexported sql*
// constants and the buildCouponListQuery / buildMCListQuery helpers (the list
// SQL is dynamic, so the tests reuse the same pure builders to predict the exact
// statement text + arg order pgxmock will see). The two atomic critical sections
// — IssueToMember (conditional quota increment) and RedeemAtomic (unused→used) —
// are the load-bearing assertions, mirrored from MemoryCouponRepo.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

var (
	fixedTime = time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) // "now" / issued_at
	validFrom = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	validTo   = time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC) // > fixedTime
	pastTime  = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)      // < fixedTime
)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// couponColumns returns the 14 coupons columns in scanCoupon order.
func couponColumns() []string {
	return []string{"id", "tenant_id", "store_id", "name", "type", "params", "valid_from", "valid_to", "total_quota", "issued", "status", "created_by", "created_at", "updated_at"}
}

// mcColumns returns the 11 member_coupons columns in scanMemberCoupon order.
func mcColumns() []string {
	return []string{"id", "tenant_id", "member_id", "coupon_id", "status", "source", "strategy_id", "issued_at", "used_at", "expire_at", "ref_order_id"}
}

func TestPGCouponRepo_Create(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	params, _ := json.Marshal(CouponParams{ThresholdCents: 1000, OffCents: 100})
	// Status empty → repo defaults to draft (defensive: avoids CHECK violation).
	mock.ExpectQuery(sqlCouponCreate).
		WithArgs(int64(1), int64(10), "满100减10", TypeFullReduction, params, validFrom, validTo, int64(100), StatusDraft, int64(5)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "created_at", "updated_at"}).AddRow(int64(42), fixedTime, fixedTime))
	c, err := repo.CreateCoupon(context.Background(), &Coupon{
		TenantID: 1, StoreID: 10, Name: "满100减10", Type: TypeFullReduction,
		Params:    CouponParams{ThresholdCents: 1000, OffCents: 100},
		ValidFrom: validFrom, ValidTo: validTo, TotalQuota: 100, Status: "", CreatedBy: 5,
	})
	require.NoError(t, err)
	require.Equal(t, int64(42), c.ID)
	require.Equal(t, StatusDraft, c.Status) // defaulted from ""
	require.Equal(t, int64(100), c.TotalQuota)
	require.Equal(t, fixedTime, c.CreatedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_GetCoupon(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// Nullable columns (store_id/total_quota/created_by = NULL) map to 0; params
	// JSONB decodes into CouponParams.
	mock.ExpectQuery(sqlCouponGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(couponColumns()).AddRow(
			int64(42), int64(1), nil, "代金5元", TypeCash, []byte(`{"off_cents":500}`),
			validFrom, validTo, nil, int64(0), StatusActive, nil, fixedTime, fixedTime,
		))
	c, err := repo.GetCoupon(context.Background(), 1, 42)
	require.NoError(t, err)
	require.Equal(t, int64(42), c.ID)
	require.Equal(t, int64(0), c.StoreID)    // NULL → 0
	require.Equal(t, int64(0), c.TotalQuota) // NULL → 0 (unlimited)
	require.Equal(t, int64(0), c.CreatedBy)  // NULL → 0
	require.Equal(t, StatusActive, c.Status)
	require.Equal(t, int64(500), c.Params.OffCents)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_GetCouponNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// Absent and cross-tenant are indistinguishable: both surface as ErrNoRows
	// because of the WHERE tenant_id filter.
	mock.ExpectQuery(sqlCouponGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetCoupon(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_GetByName(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	mock.ExpectQuery(sqlCouponGetByName).
		WithArgs(int64(1), "满100减10").
		WillReturnRows(pgxmock.NewRows(couponColumns()).AddRow(
			int64(42), int64(1), int64(10), "满100减10", TypeFullReduction, []byte(`{"threshold_cents":1000,"off_cents":100}`),
			validFrom, validTo, int64(100), int64(3), StatusActive, int64(5), fixedTime, fixedTime,
		))
	c, err := repo.GetByName(context.Background(), 1, "满100减10")
	require.NoError(t, err)
	require.Equal(t, int64(42), c.ID)
	require.Equal(t, int64(1000), c.Params.ThresholdCents)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_GetByNameNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	mock.ExpectQuery(sqlCouponGetByName).
		WithArgs(int64(1), "nope").
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetByName(context.Background(), 1, "nope")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_UpdateCoupon(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	params, _ := json.Marshal(CouponParams{ThresholdCents: 1000, OffCents: 100})
	mock.ExpectQuery(sqlCouponUpdate).
		WithArgs(int64(10), "满100减10", TypeFullReduction, params, validFrom, validTo, int64(100), StatusActive, int64(3), int64(5), int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(couponColumns()).AddRow(
			int64(42), int64(1), int64(10), "满100减10", TypeFullReduction, params,
			validFrom, validTo, int64(100), int64(3), StatusActive, int64(5), fixedTime, fixedTime,
		))
	c, err := repo.UpdateCoupon(context.Background(), &Coupon{
		ID: 42, TenantID: 1, StoreID: 10, Name: "满100减10", Type: TypeFullReduction,
		Params:    CouponParams{ThresholdCents: 1000, OffCents: 100},
		ValidFrom: validFrom, ValidTo: validTo, TotalQuota: 100, Status: StatusActive,
		Issued: 3, CreatedBy: 5,
	})
	require.NoError(t, err)
	require.Equal(t, StatusActive, c.Status)
	require.Equal(t, int64(3), c.Issued)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_UpdateCouponNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	params, _ := json.Marshal(CouponParams{})
	// Cross-tenant / absent: RETURNING yields no rows.
	mock.ExpectQuery(sqlCouponUpdate).
		WithArgs(nil, "x", TypeCash, params, validFrom, validTo, nil, StatusDraft, int64(0), nil, int64(1), int64(77)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateCoupon(context.Background(), &Coupon{
		ID: 77, TenantID: 1, Name: "x", Type: TypeCash, ValidFrom: validFrom, ValidTo: validTo,
		Status: StatusDraft,
	})
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_ListCoupons(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// Reuse the builder to predict the exact dynamic SQL + args.
	sqlStr, args := buildCouponListQuery(1, CouponFilter{Limit: 10, Status: StatusActive})
	mock.ExpectQuery(sqlStr).
		WithArgs(args...).
		WillReturnRows(pgxmock.NewRows(couponColumns()).
			AddRow(int64(2), int64(1), int64(10), "b", TypeCash, []byte(`{}`), validFrom, validTo, nil, int64(0), StatusActive, nil, fixedTime, fixedTime).
			AddRow(int64(1), int64(1), int64(10), "a", TypeCash, []byte(`{}`), validFrom, validTo, nil, int64(0), StatusActive, nil, fixedTime, fixedTime))
	got, err := repo.ListCoupons(context.Background(), 1, CouponFilter{Limit: 10, Status: StatusActive})
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(2), got[0].ID) // newest first
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_IssueToMember(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// Step 1: load coupon (active, in-window, under quota).
	mock.ExpectQuery(sqlCouponGetByID).
		WithArgs(int64(1), int64(3)).
		WillReturnRows(pgxmock.NewRows(couponColumns()).AddRow(
			int64(3), int64(1), int64(10), "券", TypeCash, []byte(`{}`),
			validFrom, validTo, int64(10), int64(3), StatusActive, int64(5), fixedTime, fixedTime,
		))
	// Step 2: conditional increment RETURNING issued (3 → 4).
	mock.ExpectQuery(sqlCouponIncrementIssued).
		WithArgs(int64(1), int64(3)).
		WillReturnRows(pgxmock.NewRows([]string{"issued"}).AddRow(int64(4)))
	// Step 3: insert member-coupon. expireAt nil → expire falls back to coupon.ValidTo.
	mock.ExpectQuery(sqlMCInsert).
		WithArgs(int64(1), int64(7), int64(3), MCStatusUnused, SourceAIStrategy, int64(55), fixedTime, validTo).
		WillReturnRows(pgxmock.NewRows(mcColumns()).AddRow(
			int64(99), int64(1), int64(7), int64(3), MCStatusUnused, SourceAIStrategy, int64(55),
			fixedTime, nil, validTo, nil,
		))
	mc, err := repo.IssueToMember(context.Background(), 1, 7, 3, SourceAIStrategy, 55, nil, fixedTime)
	require.NoError(t, err)
	require.Equal(t, int64(99), mc.ID)
	require.Equal(t, MCStatusUnused, mc.Status)
	require.Equal(t, SourceAIStrategy, mc.Source)
	require.Equal(t, int64(55), mc.StrategyID)
	require.NotNil(t, mc.ExpireAt)
	require.Equal(t, validTo, *mc.ExpireAt) // fell back to coupon.ValidTo
	require.Nil(t, mc.UsedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_IssueToMemberQuotaExhausted(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// issued(10) >= total_quota(10): Go pre-check trips before any write.
	mock.ExpectQuery(sqlCouponGetByID).
		WithArgs(int64(1), int64(3)).
		WillReturnRows(pgxmock.NewRows(couponColumns()).AddRow(
			int64(3), int64(1), int64(10), "券", TypeCash, []byte(`{}`),
			validFrom, validTo, int64(10), int64(10), StatusActive, int64(5), fixedTime, fixedTime,
		))
	_, err := repo.IssueToMember(context.Background(), 1, 7, 3, SourceManual, 0, nil, fixedTime)
	require.ErrorIs(t, err, ErrQuotaExhausted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_IssueToMemberRacedQuota(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// Pre-check passes (9 < 10) but the conditional UPDATE returns no rows:
	// a concurrent issuer took the last slot between read and update.
	mock.ExpectQuery(sqlCouponGetByID).
		WithArgs(int64(1), int64(3)).
		WillReturnRows(pgxmock.NewRows(couponColumns()).AddRow(
			int64(3), int64(1), int64(10), "券", TypeCash, []byte(`{}`),
			validFrom, validTo, int64(10), int64(9), StatusActive, int64(5), fixedTime, fixedTime,
		))
	mock.ExpectQuery(sqlCouponIncrementIssued).
		WithArgs(int64(1), int64(3)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.IssueToMember(context.Background(), 1, 7, 3, SourceManual, 0, nil, fixedTime)
	require.ErrorIs(t, err, ErrQuotaExhausted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_IssueToMemberInactive(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	mock.ExpectQuery(sqlCouponGetByID).
		WithArgs(int64(1), int64(3)).
		WillReturnRows(pgxmock.NewRows(couponColumns()).AddRow(
			int64(3), int64(1), int64(10), "券", TypeCash, []byte(`{}`),
			validFrom, validTo, int64(10), int64(0), StatusDraft, int64(5), fixedTime, fixedTime,
		))
	_, err := repo.IssueToMember(context.Background(), 1, 7, 3, SourceManual, 0, nil, fixedTime)
	require.ErrorIs(t, err, ErrCouponInactive)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_IssueToMemberNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	mock.ExpectQuery(sqlCouponGetByID).
		WithArgs(int64(1), int64(3)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.IssueToMember(context.Background(), 1, 7, 3, SourceManual, 0, nil, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_GetMemberCoupon(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	mock.ExpectQuery(sqlMCGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnRows(pgxmock.NewRows(mcColumns()).AddRow(
			int64(99), int64(1), int64(7), int64(3), MCStatusUsed, SourceManual, nil,
			fixedTime, fixedTime, validTo, int64(200),
		))
	mc, err := repo.GetMemberCoupon(context.Background(), 1, 99)
	require.NoError(t, err)
	require.Equal(t, MCStatusUsed, mc.Status)
	require.Equal(t, int64(200), mc.RefOrderID)
	require.Equal(t, int64(0), mc.StrategyID) // NULL → 0
	require.NotNil(t, mc.UsedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_GetMemberCouponNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	mock.ExpectQuery(sqlMCGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetMemberCoupon(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_ListMemberCoupons(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	sqlStr, args := buildMCListQuery("member_id", 1, 7, "")
	mock.ExpectQuery(sqlStr).
		WithArgs(args...).
		WillReturnRows(pgxmock.NewRows(mcColumns()).
			AddRow(int64(2), int64(1), int64(7), int64(3), MCStatusUnused, SourceManual, nil, fixedTime, nil, validTo, nil).
			AddRow(int64(1), int64(1), int64(7), int64(4), MCStatusUsed, SourceManual, nil, fixedTime, fixedTime, validTo, int64(9)))
	got, err := repo.ListMemberCoupons(context.Background(), 1, 7, "")
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(2), got[0].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_ListByStrategy(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	sqlStr, args := buildMCListQuery("strategy_id", 1, 55, MCStatusUnused)
	mock.ExpectQuery(sqlStr).
		WithArgs(args...).
		WillReturnRows(pgxmock.NewRows(mcColumns()).
			AddRow(int64(9), int64(1), int64(7), int64(3), MCStatusUnused, SourceAIStrategy, int64(55), fixedTime, nil, validTo, nil))
	got, err := repo.ListByStrategy(context.Background(), 1, 55, MCStatusUnused)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(55), got[0].StrategyID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_RedeemAtomic(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// Load: unused, expire in the future (no expiry trip).
	mock.ExpectQuery(sqlMCGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnRows(pgxmock.NewRows(mcColumns()).AddRow(
			int64(99), int64(1), int64(7), int64(3), MCStatusUnused, SourceManual, nil,
			fixedTime, nil, validTo, nil,
		))
	// Conditional unused→used RETURNING the used row.
	mock.ExpectQuery(sqlMCRedeem).
		WithArgs(fixedTime, int64(200), int64(1), int64(99)).
		WillReturnRows(pgxmock.NewRows(mcColumns()).AddRow(
			int64(99), int64(1), int64(7), int64(3), MCStatusUsed, SourceManual, nil,
			fixedTime, fixedTime, validTo, int64(200),
		))
	mc, err := repo.RedeemAtomic(context.Background(), 1, 99, 200, fixedTime)
	require.NoError(t, err)
	require.Equal(t, MCStatusUsed, mc.Status)
	require.NotNil(t, mc.UsedAt)
	require.Equal(t, fixedTime, *mc.UsedAt)
	require.Equal(t, int64(200), mc.RefOrderID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_RedeemAtomicUsed(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	mock.ExpectQuery(sqlMCGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnRows(pgxmock.NewRows(mcColumns()).AddRow(
			int64(99), int64(1), int64(7), int64(3), MCStatusUsed, SourceManual, nil,
			fixedTime, fixedTime, validTo, int64(200),
		))
	_, err := repo.RedeemAtomic(context.Background(), 1, 99, 200, fixedTime)
	require.ErrorIs(t, err, ErrCouponUsed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_RedeemAtomicExpired(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// ExpireAt in the past → mark expired + ErrCouponExpired (no redeem UPDATE).
	mock.ExpectQuery(sqlMCGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnRows(pgxmock.NewRows(mcColumns()).AddRow(
			int64(99), int64(1), int64(7), int64(3), MCStatusUnused, SourceManual, nil,
			fixedTime, nil, pastTime, nil,
		))
	mock.ExpectExec(sqlMCMarkExpired).
		WithArgs(int64(1), int64(99)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	_, err := repo.RedeemAtomic(context.Background(), 1, 99, 200, fixedTime)
	require.ErrorIs(t, err, ErrCouponExpired)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_RedeemAtomicExpiredFallbackToCouponValidTo(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	// ExpireAt nil → fall back to coupon.ValidTo (past) → expired.
	mock.ExpectQuery(sqlMCGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnRows(pgxmock.NewRows(mcColumns()).AddRow(
			int64(99), int64(1), int64(7), int64(3), MCStatusUnused, SourceManual, nil,
			fixedTime, nil, nil, nil,
		))
	mock.ExpectQuery(sqlCouponGetByID).
		WithArgs(int64(1), int64(3)).
		WillReturnRows(pgxmock.NewRows(couponColumns()).AddRow(
			int64(3), int64(1), int64(10), "券", TypeCash, []byte(`{}`),
			pastTime, pastTime, nil, int64(0), StatusActive, nil, fixedTime, fixedTime,
		))
	mock.ExpectExec(sqlMCMarkExpired).
		WithArgs(int64(1), int64(99)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	_, err := repo.RedeemAtomic(context.Background(), 1, 99, 200, fixedTime)
	require.ErrorIs(t, err, ErrCouponExpired)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGCouponRepo_RedeemAtomicNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGCouponRepo(mock)
	mock.ExpectQuery(sqlMCGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.RedeemAtomic(context.Background(), 1, 99, 200, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	// Nil repo / nil pool degrade to ErrNotFound / empty rather than panicking.
	repo := NewPGCouponRepo(nil)

	_, err := repo.GetCoupon(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.GetByName(context.Background(), 1, "x")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.GetMemberCoupon(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.CreateCoupon(context.Background(), &Coupon{TenantID: 1, Name: "x", Type: TypeCash})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.UpdateCoupon(context.Background(), &Coupon{ID: 1, TenantID: 1, Name: "x", Type: TypeCash})
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.IssueToMember(context.Background(), 1, 1, 1, SourceManual, 0, nil, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = repo.RedeemAtomic(context.Background(), 1, 1, 0, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)

	out, err := repo.ListCoupons(context.Background(), 1, CouponFilter{})
	require.NoError(t, err)
	require.Empty(t, out)

	mc, err := repo.ListMemberCoupons(context.Background(), 1, 1, "")
	require.NoError(t, err)
	require.Empty(t, mc)

	strat, err := repo.ListByStrategy(context.Background(), 1, 55, "")
	require.NoError(t, err)
	require.Empty(t, strat)
}
