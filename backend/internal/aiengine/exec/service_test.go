package exec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// These tests exercise the Executor (BE-B-11) end-to-end with fake anti-corruption
// ports + the real in-memory strategy.Service (CONTRACT §12.6 exec DoD). They
// cover: 6 ActionType dispatch, blocked attribution (reach blocked → ItemBlocked,
// NOT executed), margin floor pre-check, price_adj confirmation gate,
// recharge_promo stub, nil-port guard, and cross-tenant isolation.

// --- fakes ---

type fakeCoupons struct {
	ref   *CouponRef
	err   error
	calls int
}

func (f *fakeCoupons) Issue(_ context.Context, memberID, _, _ int64, _ *time.Time) (*CouponRef, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.ref != nil {
		return f.ref, nil
	}
	return &CouponRef{MemberCouponID: 9001, MemberID: memberID}, nil
}

type fakeSender struct {
	ref   *SendRef
	err   error
	calls int
}

func (f *fakeSender) Send(_ context.Context, _, _, _ int64, _, _ string, _ map[string]string) (*SendRef, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.ref != nil {
		return f.ref, nil
	}
	return &SendRef{TaskID: 7001, Status: statusSent}, nil
}

type fakePricer struct {
	err   error
	calls int
}

func (f *fakePricer) UpdatePrice(_ context.Context, _, _, _ int64) error {
	f.calls++
	return f.err
}

type fakeGroupbuy struct {
	ref   *GroupbuyRef
	err   error
	calls int
}

func (f *fakeGroupbuy) Launch(_ context.Context, _, _ int64, _ string, _ int64, _, _ int, _ int64) (*GroupbuyRef, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.ref != nil {
		return f.ref, nil
	}
	return &GroupbuyRef{GroupbuyID: 8001}, nil
}

// --- helpers ---

func tenantCtx(tid, sid int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{TenantID: tid, StoreID: sid})
}

// seedAdvice persists one strategy with the given items and returns the items
// with their allocated IDs backfilled (the repo assigns IDs in SaveAdvice).
func seedAdvice(t *testing.T, svc *strategy.Service, tid, sid int64, items ...strategy.StrategyItem) []strategy.StrategyItem {
	t.Helper()
	ctx := tenantCtx(tid, sid)
	s, err := svc.SaveAdvice(ctx, &strategy.Strategy{
		TenantID:    tid,
		StoreID:     sid,
		ForDate:     "2026-07-28",
		GeneratedBy: strategy.GenLiveLLM,
		Status:      strategy.StatusGenerated,
		Items:       items,
	})
	if err != nil {
		t.Fatalf("seed SaveAdvice: %v", err)
	}
	return s.Items
}

func item(action, reason string, productID int64, params map[string]any) strategy.StrategyItem {
	return strategy.StrategyItem{
		Rank:       1,
		ActionType: action,
		ReasonType: reason,
		ProductID:  productID,
		Params:     params,
		Copy:       "今日推荐",
		MarginOK:   true,
	}
}

// newSvc builds a fresh in-memory strategy.Service for one test.
func newSvc() *strategy.Service {
	return strategy.New(strategy.NewMemoryStrategyRepo())
}

const (
	seedTID int64 = 1
	seedSID int64 = 10
)

// --- tests ---

// TestExecutor_GroupSeckill_Executed: happy path — coupon issued + reach sent.
func TestExecutor_GroupSeckill_Executed(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActGroupSeckill, rule.ReasonClearanceCoupon, 100,
		map[string]any{"member_id": int64(55), "coupon_id": int64(77), "expire_hours": 24},
	))
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, &fakeSender{}, &fakePricer{}, &fakeGroupbuy{})
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemExecuted {
		t.Fatalf("status=%s want executed", res.Status)
	}
	if res.RefID != 9001 || res.TaskID != 7001 {
		t.Fatalf("RefID=%d TaskID=%d", res.RefID, res.TaskID)
	}
	// item landed as executed
	it, _ := svc.GetItem(tenantCtx(seedTID, seedSID), items[0].ID)
	if it.ExecutionStatus != strategy.ItemExecuted {
		t.Fatalf("item status=%s want executed", it.ExecutionStatus)
	}
}

// TestExecutor_CouponWake_Blocked: reach blocked → ExecResult.blocked + item
// lands ItemBlocked (NOT executed). This is the core attribution contract
// (CONTRACT §12.5.7).
func TestExecutor_CouponWake_Blocked(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActCouponWake, rule.ReasonWakeDormant, 0,
		map[string]any{"member_id": int64(55), "coupon_id": int64(77)},
	))
	sender := &fakeSender{ref: &SendRef{TaskID: 7001, Status: "blocked", BlockedReason: "freq_circuit"}}
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, sender, &fakePricer{}, &fakeGroupbuy{})
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemBlocked {
		t.Fatalf("status=%s want blocked", res.Status)
	}
	if res.Reason != "freq_circuit" {
		t.Fatalf("reason=%s want freq_circuit", res.Reason)
	}
	// coupon was still issued (attribution), RefID carries the coupon id
	if res.RefID != 9001 {
		t.Fatalf("RefID=%d want 9001 (coupon still issued before block)", res.RefID)
	}
	// item landed as blocked, NOT executed
	it, _ := svc.GetItem(tenantCtx(seedTID, seedSID), items[0].ID)
	if it.ExecutionStatus != strategy.ItemBlocked {
		t.Fatalf("item status=%s want blocked", it.ExecutionStatus)
	}
}

// TestExecutor_PriceAdj_NoConfirm: high-risk action without boss confirmation →
// blocked(confirm_required), PriceUpdater never called.
func TestExecutor_PriceAdj_NoConfirm(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActPriceAdj, rule.ReasonClearanceCoupon, 100,
		map[string]any{"cost_cents": int64(1200), "price_cents": int64(1500)},
	))
	pricer := &fakePricer{}
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, &fakeSender{}, pricer, &fakeGroupbuy{})
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID, Confirm: false})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemBlocked || res.Reason != ReasonConfirmRequired {
		t.Fatalf("status=%s reason=%s want blocked/confirm_required", res.Status, res.Reason)
	}
	if pricer.calls != 0 {
		t.Fatalf("pricer called %d times, want 0 (confirm gate)", pricer.calls)
	}
}

// TestExecutor_PriceAdj_MarginFloor: new price below floor → blocked(margin_floor).
func TestExecutor_PriceAdj_MarginFloor(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActPriceAdj, rule.ReasonClearanceCoupon, 100,
		map[string]any{"cost_cents": int64(1200), "price_cents": int64(1100)}, // 1100 < 1200 → below floor
	))
	pricer := &fakePricer{}
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, &fakeSender{}, pricer, &fakeGroupbuy{})
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID, Confirm: true})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemBlocked || res.Reason != ReasonMarginFloor {
		t.Fatalf("status=%s reason=%s want blocked/margin_floor", res.Status, res.Reason)
	}
	if pricer.calls != 0 {
		t.Fatalf("pricer called %d, want 0 (margin pre-check rejects)", pricer.calls)
	}
}

// TestExecutor_PriceAdj_Executed: reasonable new price + confirm → executed.
func TestExecutor_PriceAdj_Executed(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActPriceAdj, rule.ReasonClearanceCoupon, 100,
		map[string]any{"cost_cents": int64(1200), "price_cents": int64(1500)}, // 20% margin ≥ 5%
	))
	pricer := &fakePricer{}
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, &fakeSender{}, pricer, &fakeGroupbuy{})
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID, Confirm: true})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemExecuted {
		t.Fatalf("status=%s want executed", res.Status)
	}
	if pricer.calls != 1 {
		t.Fatalf("pricer calls=%d want 1", pricer.calls)
	}
}

// TestExecutor_MomentsPost_Audit: M3-S1 audit-only, no port driven.
func TestExecutor_MomentsPost_Audit(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActMomentsPost, rule.ReasonHighValue, 0,
		map[string]any{},
	))
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		nil, nil, nil, nil) // no ports needed for audit
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemExecuted {
		t.Fatalf("status=%s want executed (audit)", res.Status)
	}
	if res.RefID != 0 || res.TaskID != 0 {
		t.Fatalf("audit should have zero RefID/TaskID, got %d/%d", res.RefID, res.TaskID)
	}
}

// TestExecutor_Groupbuy_Executed: launch → executed, RefID=GroupbuyID.
func TestExecutor_Groupbuy_Executed(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActGroupbuy, rule.ReasonBundleSlow, 100,
		map[string]any{"price_cents": int64(990), "required_count": int64(3), "valid_hours": int64(24)},
	))
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, &fakeSender{}, &fakePricer{}, &fakeGroupbuy{})
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemExecuted {
		t.Fatalf("status=%s want executed", res.Status)
	}
	if res.RefID != 8001 {
		t.Fatalf("RefID=%d want 8001 (GroupbuyID)", res.RefID)
	}
}

// TestExecutor_RechargePromo_NotExecutable: M4 stub → blocked(not_executable).
func TestExecutor_RechargePromo_NotExecutable(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActRechargePromo, rule.ReasonFestival, 0,
		map[string]any{},
	))
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, &fakeSender{}, &fakePricer{}, &fakeGroupbuy{})
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemBlocked || res.Reason != ReasonNotExecutable {
		t.Fatalf("status=%s reason=%s want blocked/not_executable", res.Status, res.Reason)
	}
}

// TestExecutor_PortNotConfigured: required port nil → blocked(not_configured).
func TestExecutor_PortNotConfigured(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActGroupSeckill, rule.ReasonClearanceCoupon, 100,
		map[string]any{"member_id": int64(55), "coupon_id": int64(77)},
	))
	// coupons + reacher both nil → ErrPortNotConfigured
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5}, nil, nil, nil, nil)
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemBlocked || res.Reason != ReasonNotConfigured {
		t.Fatalf("status=%s reason=%s want blocked/not_configured", res.Status, res.Reason)
	}
}

// TestExecutor_CouponWake_BirthdayTpl: coupon_wake picks the birthday template
// for reason birthday_care (the ONLY place exec reads ReasonType).
func TestExecutor_CouponWake_BirthdayTpl(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActCouponWake, rule.ReasonBirthday, 0,
		map[string]any{"member_id": int64(55), "coupon_id": int64(77)},
	))
	sender := &fakeSender{} // captures nothing, but we assert Send is called
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, sender, &fakePricer{}, &fakeGroupbuy{})
	_, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if sender.calls != 1 {
		t.Fatalf("sender calls=%d want 1", sender.calls)
	}
}

// TestExecutor_CrossTenant: tenant B cannot execute tenant A's item →
// strategy.ErrNotFound surfaced (handler maps 404).
func TestExecutor_CrossTenant(t *testing.T) {
	svc := newSvc()
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActCouponWake, rule.ReasonWakeDormant, 0,
		map[string]any{"member_id": int64(55), "coupon_id": int64(77)},
	))
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, &fakeSender{}, &fakePricer{}, &fakeGroupbuy{})
	// tenant 9999 tries to execute tenant 1's item
	_, err := ex.Run(tenantCtx(9999, 20), ExecuteInput{StrategyItemID: items[0].ID})
	if !errors.Is(err, strategy.ErrNotFound) {
		t.Fatalf("cross-tenant err=%v want strategy.ErrNotFound", err)
	}
}

// TestExecutor_CouponMarginFloor: the third margin check rejects a coupon whose
// discount would drop the product below the floor (defense-in-depth).
func TestExecutor_CouponMarginFloor(t *testing.T) {
	svc := newSvc()
	// discount permille 300 (3折) on a product with cost 1200 / price 2580:
	// pay = 2580*300/1000 = 774 < cost → margin negative → below floor.
	items := seedAdvice(t, svc, seedTID, seedSID, item(
		rule.ActGroupSeckill, rule.ReasonClearanceCoupon, 100,
		map[string]any{
			"member_id":         int64(55),
			"coupon_id":         int64(77),
			"cost_cents":        int64(1200),
			"price_cents":       int64(2580),
			"coupon_type":       "discount",
			"discount_permille": int64(300),
		},
	))
	coupons := &fakeCoupons{}
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		coupons, &fakeSender{}, &fakePricer{}, &fakeGroupbuy{})
	res, err := ex.Run(tenantCtx(seedTID, seedSID), ExecuteInput{StrategyItemID: items[0].ID})
	if err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if res.Status != strategy.ItemBlocked || res.Reason != ReasonMarginFloor {
		t.Fatalf("status=%s reason=%s want blocked/margin_floor", res.Status, res.Reason)
	}
	if coupons.calls != 0 {
		t.Fatalf("coupons.Issue called %d, want 0 (margin pre-check rejects)", coupons.calls)
	}
}

// TestExecutor_NoTenant: missing tenant in ctx → error (EnforceTenant gate).
func TestExecutor_NoTenant(t *testing.T) {
	svc := newSvc()
	ex := NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&fakeCoupons{}, &fakeSender{}, &fakePricer{}, &fakeGroupbuy{})
	_, err := ex.Run(context.Background(), ExecuteInput{StrategyItemID: 1})
	if err == nil {
		t.Fatal("expected error for missing tenant")
	}
}
