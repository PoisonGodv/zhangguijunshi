package aiengine

// enrich_test.go covers aiengine 修复②: the audience-resolution + coupon-
// template enrichment stage, and — critically — the end-to-end integration that
// a pipeline-generated coupon-bearing item is actually executable (or degrades
// to an HONEST blocked(audience_unresolved)), plugging the "hand-injected Params
// fake green" hole the old exec-only tests left.

import (
	"context"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// --- enrichment port fakes ---

type fakeMemberResolver struct {
	id  int64
	err error
}

func (f fakeMemberResolver) ResolveMember(_ context.Context, _, _ int64, _ string) (int64, error) {
	return f.id, f.err
}

type fakeCouponSelector struct {
	id  int64
	err error
}

func (f fakeCouponSelector) SelectCoupon(_ context.Context, _, _ int64, _, _ string) (int64, error) {
	return f.id, f.err
}

// --- exec port fakes (local to this package) ---

type enrichFakeCoupons struct{ calls int }

func (f *enrichFakeCoupons) Issue(_ context.Context, memberID, _, _ int64, _ *time.Time) (*exec.CouponRef, error) {
	f.calls++
	return &exec.CouponRef{MemberCouponID: 9001, MemberID: memberID}, nil
}

type enrichFakeSender struct{ calls int }

func (f *enrichFakeSender) Send(_ context.Context, _, _, _ int64, _, _ string, _ map[string]string) (*exec.SendRef, error) {
	f.calls++
	return &exec.SendRef{TaskID: 7001, Status: "sent"}, nil
}

// TestEnrich_WritesMemberAndCoupon: the stage writes resolved member_id /
// coupon_id onto coupon-bearing items and leaves non-coupon actions untouched.
func TestEnrich_WritesMemberAndCoupon(t *testing.T) {
	en := NewEnricher(fakeMemberResolver{id: 55}, fakeCouponSelector{id: 77}, nil)
	items := []*agent.EnhancedItem{
		{Routed: rule.RoutedItem{ActionType: rule.ActCouponWake, Target: "30天未到店高价值会员"}},
		{Routed: rule.RoutedItem{ActionType: rule.ActGroupSeckill, Target: "临期SKU群秒杀"}},
		{Routed: rule.RoutedItem{ActionType: rule.ActMomentsPost, Target: "朋友圈"}}, // non-coupon
	}
	en.Enrich(ctxWith(7001, 7100), 7001, 7100, items)

	for i := 0; i < 2; i++ {
		if got := items[i].Params["member_id"]; got != int64(55) {
			t.Errorf("item[%d] member_id = %v, want 55", i, got)
		}
		if got := items[i].Params["coupon_id"]; got != int64(77) {
			t.Errorf("item[%d] coupon_id = %v, want 77", i, got)
		}
	}
	if _, ok := items[2].Params["member_id"]; ok {
		t.Errorf("non-coupon action must not get member_id: %+v", items[2].Params)
	}
}

// TestEnrich_UnresolvedLeavesKeyUnset: a resolver returning 0 (no matching
// member / no active template) must leave the key UNSET — never a fabricated 0
// id — so exec degrades honestly.
func TestEnrich_UnresolvedLeavesKeyUnset(t *testing.T) {
	en := NewEnricher(fakeMemberResolver{id: 0}, fakeCouponSelector{id: 0}, nil)
	items := []*agent.EnhancedItem{
		{Routed: rule.RoutedItem{ActionType: rule.ActCouponWake, Target: "沉睡会员"}},
	}
	en.Enrich(ctxWith(7001, 7100), 7001, 7100, items)
	if _, ok := items[0].Params["member_id"]; ok {
		t.Errorf("unresolved member must leave member_id unset: %+v", items[0].Params)
	}
	if _, ok := items[0].Params["coupon_id"]; ok {
		t.Errorf("unresolved coupon must leave coupon_id unset: %+v", items[0].Params)
	}
}

// TestPipelineToExecutor_Executed is the real integration test the task demands:
// run the FULL Pipeline.Run (signal → rule → agent → enrich → converge → encode)
// to produce a persisted group_seckill item, then call executor.Run directly on
// the returned item — asserting executed. No hand-injected Params: the member_id
// / coupon_id come ONLY from the enrichment stage, and cost/price come ONLY from
// output.Encode lifting Routed.CostCents/PriceCents. This proves the one-click
// path works end-to-end off pipeline output.
func TestPipelineToExecutor_Executed(t *testing.T) {
	bundle := &signal.Bundle{TenantID: 7001, StoreID: 7100, BusinessType: "fruit", DataQuality: 70,
		Inventory: []signal.InventorySig{{ProductID: 42, Name: "芒果", CostCents: 1200, PriceCents: 2580, DaysToExpiry: 2}}}
	en := NewEnricher(fakeMemberResolver{id: 55}, fakeCouponSelector{id: 77}, nil)
	p, svc, _ := buildPipeline(t,
		&fakeCollector{bundle: bundle},
		&fakeRouter{routed: oneRoutedItem(7001)},
		agent.New(gwWithGoodJSON(t)),
		WithEnricher(en),
	)

	advice, err := p.Run(ctxWith(7001, 7100))
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}
	if len(advice.Strategy.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(advice.Strategy.Items))
	}
	it := advice.Strategy.Items[0]
	// Enrichment wrote the audience; encode wrote the cost/price view.
	if paramInt(it.Params, "member_id") != 55 || paramInt(it.Params, "coupon_id") != 77 {
		t.Fatalf("enrichment did not reach persisted item: %+v", it.Params)
	}
	if paramInt(it.Params, "cost_cents") != 1200 || paramInt(it.Params, "price_cents") != 2580 {
		t.Fatalf("encode did not lift cost/price onto item: %+v", it.Params)
	}

	// One-click execute directly on the pipeline-produced item id.
	coupons := &enrichFakeCoupons{}
	sender := &enrichFakeSender{}
	ex := exec.NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		coupons, sender, nil, nil)
	res, err := ex.Run(ctxWith(7001, 7100), exec.ExecuteInput{StrategyItemID: it.ID})
	if err != nil {
		t.Fatalf("executor.Run: %v", err)
	}
	if res.Status != strategy.ItemExecuted {
		t.Fatalf("status = %s (reason=%s), want executed", res.Status, res.Reason)
	}
	if coupons.calls != 1 || sender.calls != 1 {
		t.Fatalf("coupon/send calls = %d/%d, want 1/1", coupons.calls, sender.calls)
	}
}

// TestPipelineToExecutor_AudienceUnresolved is the degradation half of the
// integration contract: with NO enrichment wired (the pre-fix state), the
// pipeline still produces the item, but executor.Run must decline with an HONEST
// blocked(audience_unresolved) — NOT the misleading invalid_item, and NOT a fake
// executed. This is the regression guard that the fix's degradation path is the
// one actually taken.
func TestPipelineToExecutor_AudienceUnresolved(t *testing.T) {
	bundle := &signal.Bundle{TenantID: 7001, StoreID: 7100, BusinessType: "fruit", DataQuality: 70,
		Inventory: []signal.InventorySig{{ProductID: 42, Name: "芒果", CostCents: 1200, PriceCents: 2580, DaysToExpiry: 2}}}
	// No WithEnricher — simulates an unwired / dev pipeline.
	p, svc, _ := buildPipeline(t,
		&fakeCollector{bundle: bundle},
		&fakeRouter{routed: oneRoutedItem(7001)},
		agent.New(gwWithGoodJSON(t)),
	)

	advice, err := p.Run(ctxWith(7001, 7100))
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}
	it := advice.Strategy.Items[0]
	if paramInt(it.Params, "member_id") != 0 || paramInt(it.Params, "coupon_id") != 0 {
		t.Fatalf("un-enriched item must have no member/coupon: %+v", it.Params)
	}

	ex := exec.NewExecutor(svc, margin.Guard{MarginFloorPct: 5},
		&enrichFakeCoupons{}, &enrichFakeSender{}, nil, nil)
	res, err := ex.Run(ctxWith(7001, 7100), exec.ExecuteInput{StrategyItemID: it.ID})
	if err != nil {
		t.Fatalf("executor.Run: %v", err)
	}
	if res.Status != strategy.ItemBlocked {
		t.Fatalf("status = %s, want blocked", res.Status)
	}
	if res.Reason != exec.ReasonAudienceUnresolved {
		t.Fatalf("reason = %s, want audience_unresolved (NOT invalid_item)", res.Reason)
	}
}

// paramInt is a test-local numeric param extractor (JSONB numbers arrive as
// float64; in-memory enrichment writes int64).
func paramInt(m map[string]any, key string) int64 {
	switch x := m[key].(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}
