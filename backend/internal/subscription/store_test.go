package subscription

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// ===== MemoryStore =====

func TestMemoryStore_CurrentPlan_NoRecord_Free(t *testing.T) {
	s := NewMemoryStore()
	plan, err := s.CurrentPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, PlanFree, plan)
}

func TestMemoryStore_Get_None_Nil(t *testing.T) {
	s := NewMemoryStore()
	sub, err := s.Get(context.Background(), 1)
	require.NoError(t, err)
	require.Nil(t, sub)
}

func TestMemoryStore_Upsert_CancelsOldActive(t *testing.T) {
	s := NewMemoryStore()
	r1, err := s.Upsert(context.Background(), &Subscription{
		TenantID: 1, Plan: PlanPro, Status: "active",
		PeriodStartedAt: fixedNow, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	})
	require.NoError(t, err)
	require.NotZero(t, r1.ID)
	require.Equal(t, "active", r1.Status)

	// Second Upsert → first must flip to cancelled, new active record created.
	r2, err := s.Upsert(context.Background(), &Subscription{
		TenantID: 1, Plan: PlanFlagship, Status: "active",
		PeriodStartedAt: fixedNow, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	})
	require.NoError(t, err)
	require.NotEqual(t, r1.ID, r2.ID)
	require.Equal(t, PlanFlagship, r2.Plan)

	// Current plan reflects the new active record.
	plan, err := s.CurrentPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, PlanFlagship, plan)

	// History retains both rows, prior one cancelled.
	hist, err := s.ListHistory(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, hist, 2)
	require.Equal(t, "cancelled", hist[0].Status)
	require.Equal(t, r1.ID, hist[0].ID)
	require.Equal(t, "active", hist[1].Status)
	require.Equal(t, r2.ID, hist[1].ID)
}

func TestMemoryStore_ListHistory_Empty(t *testing.T) {
	s := NewMemoryStore()
	hist, err := s.ListHistory(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, hist)
}

func TestMemoryStore_CrossTenantIsolated(t *testing.T) {
	s := NewMemoryStore()
	_, err := s.Upsert(context.Background(), &Subscription{
		TenantID: 1, Plan: PlanPro, Status: "active",
		PeriodStartedAt: fixedNow, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	})
	require.NoError(t, err)

	// tenant 2 sees free, nil, empty history (no cross-tenant leakage).
	plan, err := s.CurrentPlan(context.Background(), 2)
	require.NoError(t, err)
	require.Equal(t, PlanFree, plan)
	sub, err := s.Get(context.Background(), 2)
	require.NoError(t, err)
	require.Nil(t, sub)
	hist, err := s.ListHistory(context.Background(), 2)
	require.NoError(t, err)
	require.Empty(t, hist)
}

// ===== PGStore (pgxmock) =====

// newSubMockPool builds a pgxmock pool with plain-equality SQL matching (the
// subscription SQL contains regex metacharacters). Same-package so it can
// reference the unexported sqlSub* constants.
func newSubMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// subCols is the canonical 10-column projection used by sqlSubGet / sqlSubInsert
// / sqlSubListHistory, so each test reads rows in the same field order.
var subCols = []string{
	"id", "tenant_id", "plan", "period_started_at", "period_ends_at",
	"status", "source", "payment_ref", "created_at", "updated_at",
}

func TestPGStore_CurrentPlan(t *testing.T) {
	mock := newSubMockPool(t)
	repo := NewPGStore(mock)
	mock.ExpectQuery(sqlSubCurrentPlan).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows([]string{"plan"}).AddRow(PlanPro))
	plan, err := repo.CurrentPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, PlanPro, plan)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStore_CurrentPlan_NoRow_Free(t *testing.T) {
	mock := newSubMockPool(t)
	repo := NewPGStore(mock)
	mock.ExpectQuery(sqlSubCurrentPlan).
		WithArgs(int64(1)).
		WillReturnError(pgx.ErrNoRows)
	plan, err := repo.CurrentPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, PlanFree, plan)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStore_Get(t *testing.T) {
	mock := newSubMockPool(t)
	repo := NewPGStore(mock)
	end := fixedNow.AddDate(0, 1, 0)
	mock.ExpectQuery(sqlSubGet).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(subCols).
			AddRow(int64(7), int64(1), PlanPro, fixedNow, end, "active", "wxpay", "PAY123", fixedNow, fixedNow))
	sub, err := repo.Get(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, int64(7), sub.ID)
	require.Equal(t, PlanPro, sub.Plan)
	require.NotNil(t, sub.PeriodEndsAt)
	require.True(t, sub.PeriodEndsAt.Equal(end))
	require.Equal(t, "PAY123", sub.PaymentRef)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStore_Get_NoRow_Nil(t *testing.T) {
	mock := newSubMockPool(t)
	repo := NewPGStore(mock)
	mock.ExpectQuery(sqlSubGet).WithArgs(int64(1)).WillReturnError(pgx.ErrNoRows)
	sub, err := repo.Get(context.Background(), 1)
	require.NoError(t, err)
	require.Nil(t, sub)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStore_Get_NullableColumnsDecoded(t *testing.T) {
	mock := newSubMockPool(t)
	repo := NewPGStore(mock)
	// free row: period_ends_at NULL, payment_ref NULL.
	mock.ExpectQuery(sqlSubGet).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(subCols).
			AddRow(int64(3), int64(1), PlanFree, fixedNow, nil, "active", "manual", nil, fixedNow, fixedNow))
	sub, err := repo.Get(context.Background(), 1)
	require.NoError(t, err)
	require.Nil(t, sub.PeriodEndsAt, "NULL period_ends_at must decode to nil")
	require.Equal(t, "", sub.PaymentRef, "NULL payment_ref must decode to empty")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStore_Upsert_CancelAndInsert(t *testing.T) {
	mock := newSubMockPool(t)
	repo := NewPGStore(mock)
	end := fixedNow.AddDate(0, 1, 0)
	mock.ExpectExec(sqlSubCancelActive).
		WithArgs(int64(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// period_ends_at is *time.Time → match positionally with AnyArg (pointer
	// value identity is not asserted); the rest are exact.
	mock.ExpectQuery(sqlSubInsert).
		WithArgs(int64(1), PlanPro, fixedNow, pgxmock.AnyArg(), "active", "manual", "", fixedNow, fixedNow).
		WillReturnRows(pgxmock.NewRows(subCols).
			AddRow(int64(9), int64(1), PlanPro, fixedNow, end, "active", "manual", "", fixedNow, fixedNow))
	in := &Subscription{
		TenantID: 1, Plan: PlanPro, PeriodStartedAt: fixedNow, PeriodEndsAt: &end,
		Status: "active", Source: "manual", CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}
	out, err := repo.Upsert(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, int64(9), out.ID)
	require.Equal(t, PlanPro, out.Plan)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStore_ListHistory(t *testing.T) {
	mock := newSubMockPool(t)
	repo := NewPGStore(mock)
	mock.ExpectQuery(sqlSubListHistory).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(subCols).
			AddRow(int64(1), int64(1), PlanPro, fixedNow, nil, "cancelled", "manual", nil, fixedNow, fixedNow).
			AddRow(int64(2), int64(1), PlanFlagship, fixedNow, fixedNow.AddDate(0, 1, 0), "active", "wxpay", "PAY1", fixedNow, fixedNow))
	hist, err := repo.ListHistory(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, hist, 2)
	require.Equal(t, "cancelled", hist[0].Status)
	require.Nil(t, hist[0].PeriodEndsAt)
	require.Equal(t, "active", hist[1].Status)
	require.Equal(t, "PAY1", hist[1].PaymentRef)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStore_NilPool_ReadsAreNoOp(t *testing.T) {
	repo := NewPGStore(nil)
	plan, err := repo.CurrentPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, PlanFree, plan)
	sub, err := repo.Get(context.Background(), 1)
	require.NoError(t, err)
	require.Nil(t, sub)
	hist, err := repo.ListHistory(context.Background(), 1)
	require.NoError(t, err)
	require.Empty(t, hist)
}

func TestPGStore_NilPool_Write_ErrInternal(t *testing.T) {
	repo := NewPGStore(nil)
	_, err := repo.Upsert(context.Background(), &Subscription{
		TenantID: 1, Plan: PlanPro, Status: "active",
		PeriodStartedAt: fixedNow, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	})
	require.Error(t, err)
}

// ===== ExpireDue (CONTRACT §15.8) =====

func TestMemoryStore_ExpireDue(t *testing.T) {
	s := NewMemoryStore()
	past := fixedNow.AddDate(0, 0, -1)
	future := fixedNow.AddDate(0, 1, 0)

	// tenant 1: expired period → should flip.
	_, err := s.Upsert(context.Background(), &Subscription{
		TenantID: 1, Plan: PlanPro, Status: "active",
		PeriodStartedAt: past, PeriodEndsAt: &past, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	})
	require.NoError(t, err)
	// tenant 2: future period → stays active.
	_, err = s.Upsert(context.Background(), &Subscription{
		TenantID: 2, Plan: PlanPro, Status: "active",
		PeriodStartedAt: fixedNow, PeriodEndsAt: &future, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	})
	require.NoError(t, err)
	// tenant 3: nil period (永久/free) → never expires.
	_, err = s.Upsert(context.Background(), &Subscription{
		TenantID: 3, Plan: PlanFree, Status: "active",
		PeriodStartedAt: fixedNow, PeriodEndsAt: nil, CreatedAt: fixedNow, UpdatedAt: fixedNow,
	})
	require.NoError(t, err)

	n, err := s.ExpireDue(context.Background(), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only tenant 1's lapsed sub should expire")

	plan1, _ := s.CurrentPlan(context.Background(), 1)
	require.Equal(t, PlanFree, plan1, "expired tenant reads as free")
	plan2, _ := s.CurrentPlan(context.Background(), 2)
	require.Equal(t, PlanPro, plan2, "future tenant stays pro")
	plan3, _ := s.CurrentPlan(context.Background(), 3)
	require.Equal(t, PlanFree, plan3)

	// Idempotent: a second run expires nothing new.
	n2, err := s.ExpireDue(context.Background(), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 0, n2)
}

func TestPGStore_ExpireDue(t *testing.T) {
	mock := newSubMockPool(t)
	repo := NewPGStore(mock)
	mock.ExpectExec(sqlSubExpireDue).
		WithArgs(fixedNow).
		WillReturnResult(pgxmock.NewResult("UPDATE", 3))
	n, err := repo.ExpireDue(context.Background(), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGStore_ExpireDue_NilPool_NoOp(t *testing.T) {
	repo := NewPGStore(nil)
	n, err := repo.ExpireDue(context.Background(), fixedNow)
	require.NoError(t, err)
	require.Equal(t, 0, n)
}
