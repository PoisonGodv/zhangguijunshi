package attribution

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/strategy"
)

// fixedTime is the deterministic clock anchor for every Compute/Reward test
// (CONTRACT §14.5.6: no time.Now()).
var (
	fixedExec    = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	fixedMeasure = time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC) // +24h
	fixedNow     = func() time.Time { return fixedMeasure }
)

// ---------------------------------------------------------------------------
// fake ports (in-process, deterministic)
// ---------------------------------------------------------------------------

type fakeCoupons struct {
	tenantID, strategyID int64
	sent, redeemed       int
	err                  error
	calls                int
}

func (f *fakeCoupons) SentRedeemed(ctx context.Context, tenantID, strategyID int64) (int, int, error) {
	f.calls++
	if f.err != nil {
		return 0, 0, f.err
	}
	if tenantID != f.tenantID || strategyID != f.strategyID {
		return 0, 0, nil // cross-tenant: nothing to count
	}
	return f.sent, f.redeemed, nil
}

type fakeVisits struct {
	sinceCount int
	baseline   float64
	sinceErr   error
	baseErr    error
}

func (f *fakeVisits) SinceCount(ctx context.Context, tenantID, storeID int64, since time.Time) (int, error) {
	if f.sinceErr != nil {
		return 0, f.sinceErr
	}
	return f.sinceCount, nil
}
func (f *fakeVisits) BaselineCount(ctx context.Context, tenantID, storeID int64, window time.Duration, before time.Time) (float64, error) {
	if f.baseErr != nil {
		return 0, f.baseErr
	}
	return f.baseline, nil
}

type fakeGroupbuy struct {
	tenantID, strategyID int64
	started, succeeded   int
	err                  error
}

func (f *fakeGroupbuy) StartedSucceeded(ctx context.Context, tenantID, strategyID int64) (int, int, error) {
	if f.err != nil {
		return 0, 0, f.err
	}
	if tenantID != f.tenantID || strategyID != f.strategyID {
		return 0, 0, nil
	}
	return f.started, f.succeeded, nil
}

// fakeLoader satisfies ItemLoader; returns ErrNotFound for foreign tenants.
type fakeLoader struct {
	items map[int64]*strategy.StrategyItem
}

func (l *fakeLoader) GetItem(ctx context.Context, tenantID, itemID int64) (*strategy.StrategyItem, error) {
	it, ok := l.items[itemID]
	if !ok || it == nil || it.TenantID != tenantID {
		return nil, strategy.ErrNotFound
	}
	return it, nil
}

// newItem builds a StrategyItem with the given tenant/strategy/action and an
// executed_at stamp.
func newItem(tenantID, strategyID, itemID int64, action string) *strategy.StrategyItem {
	ex := fixedExec
	return &strategy.StrategyItem{
		ID:              itemID,
		TenantID:        tenantID,
		StrategyID:      strategyID,
		Rank:            1,
		ActionType:      action,
		ExecutionStatus: strategy.ItemExecuted,
		ExecutedAt:      &ex,
	}
}

// ===========================================================================
// MemoryRepo — Record / List / LatestPerMetric + cross-tenant isolation
// ===========================================================================

func TestMemoryRepo_RecordInsertAndIDBackfill(t *testing.T) {
	r := NewMemoryRepo()
	m, err := r.Record(context.Background(), 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	require.NoError(t, err)
	require.Equal(t, int64(1), m.ID)
	require.Equal(t, int64(1), m.TenantID)
	require.Equal(t, int64(10), m.StrategyItemID)
	require.Equal(t, MetricCouponRedeem, m.Metric)
	require.InDelta(t, 0.5, m.Value, 1e-9)
	require.True(t, m.MeasuredAt.Equal(fixedMeasure))
}

func TestMemoryRepo_RecordUpsertOnSameTriple(t *testing.T) {
	r := NewMemoryRepo()
	first, err := r.Record(context.Background(), 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	require.NoError(t, err)
	// Same (tenant,item,metric,measured_at) → overwrite value, keep ID.
	second, err := r.Record(context.Background(), 1, 10, MetricCouponRedeem, 0.9, fixedMeasure)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "upsert must preserve the row id")
	require.InDelta(t, 0.9, second.Value, 1e-9)

	// Different measured_at → new row, new id.
	third, err := r.Record(context.Background(), 1, 10, MetricCouponRedeem, 0.2, fixedMeasure.Add(24*time.Hour))
	require.NoError(t, err)
	require.NotEqual(t, first.ID, third.ID)
}

func TestMemoryRepo_RecordUnknownMetric(t *testing.T) {
	r := NewMemoryRepo()
	_, err := r.Record(context.Background(), 1, 10, "no_such_metric", 0.5, fixedMeasure)
	require.ErrorIs(t, err, ErrParam)
}

func TestMemoryRepo_ListFiltersByTenant(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	_, _ = r.Record(ctx, 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	_, _ = r.Record(ctx, 1, 10, MetricVisitUplift, 3.0, fixedMeasure)
	_, _ = r.Record(ctx, 2, 20, MetricCouponRedeem, 0.7, fixedMeasure) // foreign tenant

	got, err := r.List(ctx, 1, MeasurementFilter{})
	require.NoError(t, err)
	require.Len(t, got, 2, "tenant 2 records must not leak")
	for _, m := range got {
		require.Equal(t, int64(1), m.TenantID)
	}
}

func TestMemoryRepo_ListFiltersByItemAndMetric(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	_, _ = r.Record(ctx, 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	_, _ = r.Record(ctx, 1, 11, MetricVisitUplift, 3.0, fixedMeasure)
	_, _ = r.Record(ctx, 1, 10, MetricGroupbuySuccess, 0.4, fixedMeasure)

	got, err := r.List(ctx, 1, MeasurementFilter{ItemIDs: []int64{10}, Metrics: []string{MetricCouponRedeem}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(10), got[0].StrategyItemID)
	require.Equal(t, MetricCouponRedeem, got[0].Metric)
}

func TestMemoryRepo_ListDefaultLimitAndOrder(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	// 60 rows → exceeds default limit 50.
	for i := 0; i < 60; i++ {
		_, _ = r.Record(ctx, 1, int64(i), MetricCouponRedeem, float64(i), fixedMeasure)
	}
	got, err := r.List(ctx, 1, MeasurementFilter{})
	require.NoError(t, err)
	require.Len(t, got, 50)
	// Ordered by itemID ascending.
	require.Equal(t, int64(0), got[0].StrategyItemID)
}

func TestMemoryRepo_LatestPerMetricPicksMostRecent(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	_, _ = r.Record(ctx, 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	_, _ = r.Record(ctx, 1, 10, MetricCouponRedeem, 0.9, fixedMeasure.Add(24*time.Hour)) // newer
	_, _ = r.Record(ctx, 1, 10, MetricVisitUplift, 3.0, fixedMeasure)

	got, err := r.LatestPerMetric(ctx, 1, 10)
	require.NoError(t, err)
	require.InDelta(t, 0.9, got[MetricCouponRedeem], 1e-9, "must pick the newest coupon value")
	require.InDelta(t, 3.0, got[MetricVisitUplift], 1e-9)
	_, hasGroup := got[MetricGroupbuySuccess]
	require.False(t, hasGroup, "missing metric must be omitted, not zero")
}

func TestMemoryRepo_LatestPerMetricCrossTenantEmpty(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	_, _ = r.Record(ctx, 1, 10, MetricCouponRedeem, 0.5, fixedMeasure)
	got, err := r.LatestPerMetric(ctx, 2, 10)
	require.NoError(t, err)
	require.Empty(t, got, "foreign tenant must see nothing")
}

func TestMemoryRepo_LatestPerMetricUnknownItemEmpty(t *testing.T) {
	r := NewMemoryRepo()
	got, err := r.LatestPerMetric(context.Background(), 1, 999)
	require.NoError(t, err)
	require.Empty(t, got)
}

// ===========================================================================
// Computer.Compute — coupon / visit / groupbuy + sent==0 / started==0 +
// cross-tenant + nil ports
// ===========================================================================

func TestCompute_CouponRedeemForCouponWake(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 4}
	c := NewComputer(Sources{Coupons: coupons, Visits: &fakeVisits{sinceCount: 5, baseline: 3}}, nil)
	item := newItem(1, 100, 10, "coupon_wake")
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Len(t, got, 2)
	var couponM, visitM *Measurement
	for i := range got {
		switch got[i].Metric {
		case MetricCouponRedeem:
			couponM = &got[i]
		case MetricVisitUplift:
			visitM = &got[i]
		}
	}
	require.NotNil(t, couponM)
	require.InDelta(t, 0.4, couponM.Value, 1e-9) // 4/10
	require.True(t, couponM.MeasuredAt.Equal(fixedMeasure))
	require.NotNil(t, visitM)
	require.InDelta(t, 2.0, visitM.Value, 1e-9) // since(5) - baseline(3)
	require.Equal(t, 1, coupons.calls, "coupon port queried once for coupon_wake")
}

func TestCompute_CouponRedeemSkippedWhenSentZero(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 0, redeemed: 0}
	c := NewComputer(Sources{Coupons: coupons, Visits: &fakeVisits{sinceCount: 5, baseline: 3}}, nil)
	item := newItem(1, 100, 10, "coupon_wake")
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Len(t, got, 1, "coupon_redeem must be skipped when sent==0")
	require.Equal(t, MetricVisitUplift, got[0].Metric)
}

func TestCompute_CouponPortNotQueriedForNonCouponAction(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 4}
	c := NewComputer(Sources{Coupons: coupons, Visits: &fakeVisits{sinceCount: 5, baseline: 3}}, nil)
	item := newItem(1, 100, 10, "moments_post") // not a coupon channel
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, MetricVisitUplift, got[0].Metric)
	require.Equal(t, 0, coupons.calls, "coupon port must NOT be queried for non-coupon action")
}

func TestCompute_GroupbuySuccess(t *testing.T) {
	gb := &fakeGroupbuy{tenantID: 1, strategyID: 100, started: 5, succeeded: 3}
	c := NewComputer(Sources{Groupbuy: gb, Visits: &fakeVisits{sinceCount: 5, baseline: 3}}, nil)
	item := newItem(1, 100, 10, "groupbuy")
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Len(t, got, 2)
	var gbM *Measurement
	for i := range got {
		if got[i].Metric == MetricGroupbuySuccess {
			gbM = &got[i]
		}
	}
	require.NotNil(t, gbM)
	require.InDelta(t, 0.6, gbM.Value, 1e-9) // 3/5
}

func TestCompute_GroupbuySkippedWhenStartedZero(t *testing.T) {
	gb := &fakeGroupbuy{tenantID: 1, strategyID: 100, started: 0, succeeded: 0}
	c := NewComputer(Sources{Groupbuy: gb, Visits: &fakeVisits{sinceCount: 5, baseline: 3}}, nil)
	item := newItem(1, 100, 10, "groupbuy")
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, MetricVisitUplift, got[0].Metric)
}

func TestCompute_VisitUpliftUsesWindow(t *testing.T) {
	visits := &fakeVisits{sinceCount: 8, baseline: 2.0}
	c := NewComputer(Sources{Visits: visits}, nil)
	item := newItem(1, 100, 10, "price_adj")
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.InDelta(t, 6.0, got[0].Value, 1e-9) // 8 - 2
}

func TestCompute_VisitSkippedWhenNonPositiveWindow(t *testing.T) {
	visits := &fakeVisits{sinceCount: 8, baseline: 2.0}
	c := NewComputer(Sources{Visits: visits}, nil)
	item := newItem(1, 100, 10, "price_adj")
	// measuredAt == executedAt → window 0 → visit_uplift skipped.
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedExec)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestCompute_AllNilPortsReturnsEmpty(t *testing.T) {
	c := NewComputer(Sources{}, nil) // no ports
	item := newItem(1, 100, 10, "coupon_wake")
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Empty(t, got, "nil ports must skip every metric, not panic")
}

func TestCompute_PortErrorDoesNotEmitMetric(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 4, err: errors.New("marketing down")}
	visits := &fakeVisits{sinceCount: 5, baseline: 3, baseErr: errors.New("billing down")}
	c := NewComputer(Sources{Coupons: coupons, Visits: visits}, nil)
	item := newItem(1, 100, 10, "coupon_wake")
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err, "port errors are swallowed (best-effort), not surfaced")
	require.Empty(t, got, "errored ports emit nothing")
}

func TestCompute_CrossTenantReturnsEmpty(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 4}
	visits := &fakeVisits{sinceCount: 5, baseline: 3}
	gb := &fakeGroupbuy{tenantID: 1, strategyID: 100, started: 5, succeeded: 3}
	c := NewComputer(Sources{Coupons: coupons, Visits: visits, Groupbuy: gb}, nil)
	item := newItem(1, 100, 10, "groupbuy") // item belongs to tenant 1
	got, err := c.Compute(context.Background(), 999, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Empty(t, got, "cross-tenant must emit nothing")
	require.Equal(t, 0, coupons.calls, "no port queried for a foreign-tenant item")
}

func TestCompute_NilItemErrParam(t *testing.T) {
	c := NewComputer(Sources{Visits: &fakeVisits{}}, nil)
	got, err := c.Compute(context.Background(), 1, nil, 7, fixedExec, fixedMeasure)
	require.ErrorIs(t, err, ErrParam)
	require.Nil(t, got)
}

func TestCompute_RechargePromoQueriesCoupons(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 8, redeemed: 2}
	c := NewComputer(Sources{Coupons: coupons}, nil)
	item := newItem(1, 100, 10, "recharge_promo")
	got, err := c.Compute(context.Background(), 1, item, 7, fixedExec, fixedMeasure)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, MetricCouponRedeem, got[0].Metric)
	require.InDelta(t, 0.25, got[0].Value, 1e-9) // 2/8
}

// ===========================================================================
// Computer.Reward — normalisation + clip + cross-tenant + RewardSource shape
// ===========================================================================

func TestReward_CouponAndVisitBlend(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 8} // coupon_rate 0.8
	visits := &fakeVisits{sinceCount: 12, baseline: 10.0}                        // delta 2 / max(1,10)=0.2
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: newItem(1, 100, 10, "coupon_wake")}}
	c := NewComputer(Sources{Coupons: coupons, Visits: visits}, loader, WithNowFunc(fixedNow))
	// executedAt=fixedExec, measuredAt=fixedNow()=fixedMeasure (+24h → window>0)
	r, err := c.Reward(context.Background(), 1, 10, 100, 7)
	require.NoError(t, err)
	// 0.7*0.8 + 0.3*0.2 = 0.56 + 0.06 = 0.62
	require.InDelta(t, 0.62, r, 1e-9)
}

func TestReward_NoCouponDataZerosCouponTerm(t *testing.T) {
	// moments_post: not a coupon channel → coupon_rate=0.
	visits := &fakeVisits{sinceCount: 12, baseline: 10.0}
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: newItem(1, 100, 10, "moments_post")}}
	c := NewComputer(Sources{Visits: visits}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 1, 10, 100, 7)
	require.NoError(t, err)
	// 0.7*0 + 0.3*0.2 = 0.06
	require.InDelta(t, 0.06, r, 1e-9)
}

func TestReward_ClipsAboveOne(t *testing.T) {
	// coupon_rate=1 (10/10), visit_delta_norm huge (delta 1000 / max(1,1)=1000).
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 10}
	visits := &fakeVisits{sinceCount: 1001, baseline: 1.0}
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: newItem(1, 100, 10, "coupon_wake")}}
	c := NewComputer(Sources{Coupons: coupons, Visits: visits}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 1, 10, 100, 7)
	require.NoError(t, err)
	require.InDelta(t, 1.0, r, 1e-9, "reward must clip to 1")
}

func TestReward_ClipsBelowZero(t *testing.T) {
	// coupon_rate=0, visit_delta negative and large → visit_norm negative → reward<0.
	visits := &fakeVisits{sinceCount: 0, baseline: 5.0} // delta -5 / max(1,5)=5 → -1 → clip 0
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: newItem(1, 100, 10, "price_adj")}}
	c := NewComputer(Sources{Visits: visits}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 1, 10, 100, 7)
	require.NoError(t, err)
	require.InDelta(t, 0.0, r, 1e-9, "reward must clip to 0")
}

func TestReward_SentZeroZerosCouponTerm(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 0, redeemed: 0}
	visits := &fakeVisits{sinceCount: 12, baseline: 10.0}
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: newItem(1, 100, 10, "coupon_wake")}}
	c := NewComputer(Sources{Coupons: coupons, Visits: visits}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 1, 10, 100, 7)
	require.NoError(t, err)
	require.InDelta(t, 0.06, r, 1e-9) // only visit term
}

func TestReward_AllNilPortsReturnsZero(t *testing.T) {
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: newItem(1, 100, 10, "coupon_wake")}}
	c := NewComputer(Sources{}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 1, 10, 100, 7)
	require.NoError(t, err)
	require.InDelta(t, 0.0, r, 1e-9)
}

func TestReward_CrossTenantReturnsNotFound(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 8}
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: newItem(1, 100, 10, "coupon_wake")}}
	c := NewComputer(Sources{Coupons: coupons}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 999, 10, 100, 7)
	require.ErrorIs(t, err, strategy.ErrNotFound, "cross-tenant must surface not-found")
	require.InDelta(t, 0.0, r, 1e-9)
}

func TestReward_MissingItemReturnsNotFound(t *testing.T) {
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{}}
	c := NewComputer(Sources{}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 1, 999, 100, 7)
	require.ErrorIs(t, err, strategy.ErrNotFound)
	require.InDelta(t, 0.0, r, 1e-9)
}

func TestReward_NilExecutedAtZerosVisitTerm(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 8}
	item := newItem(1, 100, 10, "coupon_wake")
	item.ExecutedAt = nil // not executed → visit term unavailable
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: item}}
	c := NewComputer(Sources{Coupons: coupons, Visits: &fakeVisits{sinceCount: 12, baseline: 10}}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 1, 10, 100, 7)
	require.NoError(t, err)
	require.InDelta(t, 0.56, r, 1e-9) // 0.7*0.8 only
}

// Compile-time: Computer satisfies bandit.RewardSource shape (already asserted
// in computer.go); this test documents the reward∈[0,1] contract end-to-end.
func TestReward_AlwaysInUnitInterval(t *testing.T) {
	coupons := &fakeCoupons{tenantID: 1, strategyID: 100, sent: 10, redeemed: 10}
	visits := &fakeVisits{sinceCount: 9999, baseline: 0.0001}
	loader := &fakeLoader{items: map[int64]*strategy.StrategyItem{10: newItem(1, 100, 10, "coupon_wake")}}
	c := NewComputer(Sources{Coupons: coupons, Visits: visits}, loader, WithNowFunc(fixedNow))
	r, err := c.Reward(context.Background(), 1, 10, 100, 7)
	require.NoError(t, err)
	require.LessOrEqual(t, r, 1.0)
	require.GreaterOrEqual(t, r, 0.0)
}

// ===========================================================================
// Concurrency (-race): MemoryRepo under parallel Record/List.
// ===========================================================================

func TestMemoryRepo_ConcurrentRecordRaceFree(t *testing.T) {
	r := NewMemoryRepo()
	ctx := context.Background()
	done := make(chan struct{})
	for w := 0; w < 8; w++ {
		go func(off int) {
			for i := 0; i < 100; i++ {
				_, _ = r.Record(ctx, int64(off), int64(off*1000+i), MetricCouponRedeem, float64(i), fixedMeasure)
			}
			done <- struct{}{}
		}(w)
	}
	for w := 0; w < 8; w++ {
		<-done
	}
	// Each tenant should see exactly 100 rows.
	for off := 0; off < 8; off++ {
		got, err := r.List(ctx, int64(off), MeasurementFilter{Limit: 1000})
		require.NoError(t, err)
		require.Len(t, got, 100)
	}
}
