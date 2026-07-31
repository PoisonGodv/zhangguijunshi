package pregen

// bandit_credit_test.go covers aiengine 修复③: the BanditUpdate de-duplication
// that stops consecutive 02:00 ticks from re-crediting the same executed item
// (arm.Trials inflation). The headline regression test runs BanditUpdate twice
// over the SAME item and asserts the arm's Trials increments only once.

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhangguijunshi/backend/internal/aiengine/bandit"
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// --- fakes ---

// banditFakeEnum is a StrategyItemEnumerator that always returns the same
// executed item(s) — simulating the 72h window re-listing an item every tick.
type banditFakeEnum struct {
	items []ExecutedItem
}

func (f *banditFakeEnum) ListExecutedSince(_ context.Context, _ int64, _ time.Time, _ int) ([]ExecutedItem, error) {
	return f.items, nil
}

// banditFakeTenants is a TenantEnumerator returning a fixed tenant list.
type banditFakeTenants struct{ ids []int64 }

func (f *banditFakeTenants) ActiveTenantIDs(_ context.Context) ([]int64, error) { return f.ids, nil }

// banditFakeResolver resolves every item to a fixed ruleID (the arm key).
type banditFakeResolver struct{ ruleID string }

func (f *banditFakeResolver) Resolve(_ context.Context, _, _ int64) (string, int64, int64, error) {
	return f.ruleID, 1, 1, nil
}

// banditFakeReward returns a fixed reward.
type banditFakeReward struct{ reward float64 }

func (f *banditFakeReward) Reward(_ context.Context, _, _, _, _ int64) (float64, error) {
	return f.reward, nil
}

// --- tests ---

// TestBanditUpdate_DedupAcrossTicks is THE regression test for 修复③: the same
// executed item is enumerated on two consecutive BanditUpdate ticks (the 72h
// window re-lists it), but the de-duplication store ensures it is credited —
// and arm.Trials incremented — exactly ONCE.
func TestBanditUpdate_DedupAcrossTicks(t *testing.T) {
	executedAt := time.Now().Add(-1 * time.Hour)
	item := &strategy.StrategyItem{ID: 42, TenantID: 1, ExecutionStatus: strategy.ItemExecuted, ExecutedAt: &executedAt}
	enum := &banditFakeEnum{items: []ExecutedItem{{Item: item, StoreID: 10}}}

	repo := bandit.NewMemoryRepo()
	learner := bandit.NewLearner(repo, bandit.DefaultConfig(),
		bandit.WithRewardSource(&banditFakeReward{reward: 1.0}),
		bandit.WithItemResolver(&banditFakeResolver{ruleID: "clearance_coupon"}),
	)
	creditStore := NewMemoryBanditCreditStore(enum)

	s := NewScheduler(nil, nil, nil, &banditFakeTenants{ids: []int64{1}},
		WithBanditLearner(learner),
		WithItemEnumerator(enum),
		WithBanditCreditStore(creditStore),
	)

	// First tick: item credited → Trials 1.
	n1, err := s.BanditUpdate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n1, "first tick credits the item")

	arm1, err := repo.Get(context.Background(), 1, "clearance_coupon")
	require.NoError(t, err)
	assert.Equal(t, 1, arm1.Trials, "after first tick Trials=1")

	// Second tick: the enumerator STILL returns the item (72h window), but the
	// credit store has marked it → it must be skipped.
	n2, err := s.BanditUpdate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "second tick must NOT re-credit the same item")

	arm2, err := repo.Get(context.Background(), 1, "clearance_coupon")
	require.NoError(t, err)
	assert.Equal(t, 1, arm2.Trials, "Trials must stay 1 after the second tick (de-dup)")
}

// TestBanditUpdate_NoCreditStore_LegacyReCredits documents the pre-fix (legacy)
// behaviour: without a BanditCreditStore the same item IS re-credited every tick
// (Trials inflates). This guards that the de-dup is opt-in and backward-compat.
func TestBanditUpdate_NoCreditStore_LegacyReCredits(t *testing.T) {
	executedAt := time.Now().Add(-1 * time.Hour)
	item := &strategy.StrategyItem{ID: 42, TenantID: 1, ExecutionStatus: strategy.ItemExecuted, ExecutedAt: &executedAt}
	enum := &banditFakeEnum{items: []ExecutedItem{{Item: item, StoreID: 10}}}

	repo := bandit.NewMemoryRepo()
	learner := bandit.NewLearner(repo, bandit.DefaultConfig(),
		bandit.WithRewardSource(&banditFakeReward{reward: 1.0}),
		bandit.WithItemResolver(&banditFakeResolver{ruleID: "clearance_coupon"}),
	)
	s := NewScheduler(nil, nil, nil, &banditFakeTenants{ids: []int64{1}},
		WithBanditLearner(learner),
		WithItemEnumerator(enum),
		// NO WithBanditCreditStore → legacy path.
	)

	_, _ = s.BanditUpdate(context.Background())
	_, _ = s.BanditUpdate(context.Background())

	arm, err := repo.Get(context.Background(), 1, "clearance_coupon")
	require.NoError(t, err)
	assert.Equal(t, 2, arm.Trials, "legacy path re-credits (the bug 修复③ fixes)")
}

// --- PG store tests (pgxmock) ---

func newCreditMock(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

func TestPGBanditCreditStore_ListUncredited(t *testing.T) {
	mock := newCreditMock(t)
	store := NewPGBanditCreditStore(mock)
	since := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	execAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(sqlListUncredited).
		WithArgs(int64(1), since, 500).
		WillReturnRows(pgxmock.NewRows([]string{"id", "executed_at", "store_id"}).
			AddRow(int64(42), &execAt, int64(10)))

	items, err := store.ListUncreditedExecuted(context.Background(), 1, since, 500)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, int64(42), items[0].Item.ID)
	assert.Equal(t, int64(10), items[0].StoreID)
	require.NotNil(t, items[0].Item.ExecutedAt)
	assert.True(t, items[0].Item.ExecutedAt.Equal(execAt))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBanditCreditStore_MarkCredited(t *testing.T) {
	mock := newCreditMock(t)
	store := NewPGBanditCreditStore(mock)
	at := time.Date(2026, 7, 30, 2, 0, 0, 0, time.UTC)

	mock.ExpectExec(sqlMarkCredited).
		WithArgs(int64(1), int64(42), at).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	require.NoError(t, store.MarkCredited(context.Background(), 1, 42, at))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGBanditCreditStore_NilPoolNoop(t *testing.T) {
	store := NewPGBanditCreditStore(nil)
	items, err := store.ListUncreditedExecuted(context.Background(), 1, time.Now(), 10)
	assert.NoError(t, err)
	assert.Nil(t, items)
	assert.NoError(t, store.MarkCredited(context.Background(), 1, 42, time.Now()))
}
