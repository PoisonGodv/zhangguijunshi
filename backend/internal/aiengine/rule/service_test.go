package rule

import (
	"context"
	"testing"

	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/margin"
)

// ===== ReasonToAction frozen mapping (CONTRACT §12.3.0) =====

func TestReasonToAction_AllSevenMappings(t *testing.T) {
	want := map[string]string{
		ReasonClearanceCoupon: ActGroupSeckill,
		ReasonRainDelivery:    ActGroupSeckill,
		ReasonBundleSlow:      ActGroupSeckill,
		ReasonWakeDormant:     ActCouponWake,
		ReasonFestival:        ActCouponWake,
		ReasonBirthday:        ActCouponWake,
		ReasonHighValue:       ActCouponWake,
	}
	if len(ReasonToAction) != len(want) {
		t.Errorf("ReasonToAction size: got %d want %d", len(ReasonToAction), len(want))
	}
	for reason, wantAct := range want {
		got, ok := ReasonToAction[reason]
		if !ok {
			t.Errorf("missing mapping for %q", reason)
			continue
		}
		if got != wantAct {
			t.Errorf("ReasonToAction[%q]: got %q want %q", reason, got, wantAct)
		}
	}
}

// ===== 5 business seeds each hit one rule (ReasonType + ActionType via map) =====

func TestRoute_Fruit_ClearanceHits(t *testing.T) {
	eng := NewEngine(DefaultSeeds(), margin.Guard{MarginFloorPct: 5})
	b := &signal.Bundle{
		TenantID:     50021,
		BusinessType: "fruit",
		Weather:      signal.Weather{Condition: "normal"},
		Holiday:      signal.Holiday{Name: "无", DaysTo: -1},
		Inventory: []signal.InventorySig{
			{ProductID: 1001, Name: "阳光玫瑰葡萄", CostCents: 1200, PriceCents: 2580, DaysToExpiry: 2},
		},
	}
	routed, err := eng.Route(context.Background(), b)
	if err != nil {
		t.Fatalf("Route err: %v", err)
	}
	if len(routed.Items) != 1 {
		t.Fatalf("expect 1 item, got %d: %+v", len(routed.Items), routed.Items)
	}
	it := routed.Items[0]
	if it.ReasonType != ReasonClearanceCoupon || it.ActionType != ActGroupSeckill {
		t.Errorf("got reason=%q action=%q", it.ReasonType, it.ActionType)
	}
	if it.CostCeilCents != 1260 {
		t.Errorf("CostCeilCents: got %d want 1260", it.CostCeilCents)
	}
	if it.Product != "阳光玫瑰葡萄" {
		t.Errorf("Product: got %q", it.Product)
	}
}

func TestRoute_Fresh_RainHits(t *testing.T) {
	eng := NewEngine(DefaultSeeds(), margin.Guard{MarginFloorPct: 5})
	b := &signal.Bundle{
		TenantID:     50021,
		BusinessType: "fresh",
		Weather:      signal.Weather{Condition: "rain"},
		Holiday:      signal.Holiday{Name: "无", DaysTo: -1},
		Inventory: []signal.InventorySig{
			{ProductID: 2001, Name: "青菜", CostCents: 1000, PriceCents: 2000, DaysToExpiry: -1},
		},
	}
	routed, err := eng.Route(context.Background(), b)
	if err != nil {
		t.Fatalf("Route err: %v", err)
	}
	if len(routed.Items) != 1 {
		t.Fatalf("expect 1 item, got %d", len(routed.Items))
	}
	it := routed.Items[0]
	if it.ReasonType != ReasonRainDelivery || it.ActionType != ActGroupSeckill {
		t.Errorf("got reason=%q action=%q", it.ReasonType, it.ActionType)
	}
}

func TestRoute_Convenience_BundleHits(t *testing.T) {
	eng := NewEngine(DefaultSeeds(), margin.Guard{MarginFloorPct: 8})
	b := &signal.Bundle{
		TenantID:     50021,
		BusinessType: "convenience",
		Weather:      signal.Weather{Condition: "normal"},
		Holiday:      signal.Holiday{Name: "无", DaysTo: -1},
		Inventory: []signal.InventorySig{
			{ProductID: 3001, Name: "玻璃水", CostCents: 5000, PriceCents: 8000, SlowMoving: true},
		},
	}
	routed, err := eng.Route(context.Background(), b)
	if err != nil {
		t.Fatalf("Route err: %v", err)
	}
	if len(routed.Items) != 1 {
		t.Fatalf("expect 1 item, got %d", len(routed.Items))
	}
	it := routed.Items[0]
	if it.ReasonType != ReasonBundleSlow || it.ActionType != ActGroupSeckill {
		t.Errorf("got reason=%q action=%q", it.ReasonType, it.ActionType)
	}
}

func TestRoute_Maternity_WakeHits(t *testing.T) {
	eng := NewEngine(DefaultSeeds(), margin.Guard{MarginFloorPct: 0})
	b := &signal.Bundle{
		TenantID:     50021,
		BusinessType: "maternity",
		Weather:      signal.Weather{Condition: "normal"},
		Holiday:      signal.Holiday{Name: "无", DaysTo: -1},
		Members:      signal.MemberStats{Sleep30: 15}, // birthday_today empty → birthday rule skips
	}
	routed, err := eng.Route(context.Background(), b)
	if err != nil {
		t.Fatalf("Route err: %v", err)
	}
	if len(routed.Items) != 1 {
		t.Fatalf("expect 1 item, got %d", len(routed.Items))
	}
	it := routed.Items[0]
	if it.ReasonType != ReasonWakeDormant || it.ActionType != ActCouponWake {
		t.Errorf("got reason=%q action=%q", it.ReasonType, it.ActionType)
	}
	// placeholder: no product binding
	if it.Product != "-" || it.ProductID != 0 || it.CostCeilCents != 0 {
		t.Errorf("placeholder fields wrong: %+v", it)
	}
}

func TestRoute_Apparel_HighValueHits(t *testing.T) {
	eng := NewEngine(DefaultSeeds(), margin.Guard{MarginFloorPct: 15})
	b := &signal.Bundle{
		TenantID:     50021,
		BusinessType: "apparel",
		Weather:      signal.Weather{Condition: "normal"},
		Holiday:      signal.Holiday{Name: "无", DaysTo: -1},
		Members:      signal.MemberStats{HighValue: []int64{101, 102}},
	}
	routed, err := eng.Route(context.Background(), b)
	if err != nil {
		t.Fatalf("Route err: %v", err)
	}
	if len(routed.Items) != 1 {
		t.Fatalf("expect 1 item, got %d", len(routed.Items))
	}
	it := routed.Items[0]
	if it.ReasonType != ReasonHighValue || it.ActionType != ActCouponWake {
		t.Errorf("got reason=%q action=%q", it.ReasonType, it.ActionType)
	}
}

// ===== Margin guardrail key case (CONTRACT §12.6 DoD) =====

func TestApplyGuardrail_ClampsDiscountToFloor(t *testing.T) {
	// cost=1200, price=2580, floor=5% → nominal ceil = 1260.
	inv := signal.InventorySig{ProductID: 1001, Name: "阳光玫瑰葡萄", CostCents: 1200, PriceCents: 2580, DaysToExpiry: 2}
	// Rule permits a breaching 4折 (permille 400 → 1032 cents < 1260).
	rl := Rule{
		ID:       "clearance_fruit_3d",
		Priority: 50,
		Action: Action{
			ReasonType: ReasonClearanceCoupon,
			Target:     "临期SKU群秒杀",
			ParamRange: ParamRange{DiscountMinPermille: 400, DiscountMaxPermille: 700, LimitQtyMax: 50},
		},
	}
	item, ok := ApplyGuardrail(inv, rl, margin.Guard{MarginFloorPct: 5})
	if !ok {
		t.Fatal("expect ok=true: clamp the discount back to the floor, do not reject")
	}
	if item.CostCeilCents != 1260 {
		t.Errorf("CostCeilCents: got %d want 1260", item.CostCeilCents)
	}
	if item.ParamRange.DiscountMinPermille != 490 {
		t.Errorf("DiscountMinPermille clamped: got %d want 490 (floor-respecting)", item.ParamRange.DiscountMinPermille)
	}
	if item.ParamRange.DiscountMaxPermille != 700 {
		t.Errorf("DiscountMaxPermille must be preserved: got %d want 700", item.ParamRange.DiscountMaxPermille)
	}
	if item.ActionType != ActGroupSeckill || item.ReasonType != ReasonClearanceCoupon {
		t.Errorf("action/reason wrong: %q/%q", item.ActionType, item.ReasonType)
	}
	// The clamped min must actually clear the margin library (defense-in-depth).
	newPrice := inv.PriceCents * item.ParamRange.DiscountMinPermille / 1000
	if err := margin.ValidatePriceAdj(margin.Guard{MarginFloorPct: 5}, inv.CostCents, newPrice); err != nil {
		t.Errorf("clamped newPrice=%d must pass guard: %v", newPrice, err)
	}
}

func TestApplyGuardrail_RuleMinAboveFloor_NoClamp(t *testing.T) {
	// rule's own min (500) already above floor-respecting permille (490) → unchanged.
	inv := signal.InventorySig{ProductID: 1, Name: "葡萄", CostCents: 1200, PriceCents: 2580}
	rl := Rule{ID: "r", Action: Action{ReasonType: ReasonClearanceCoupon, ParamRange: ParamRange{DiscountMinPermille: 500, DiscountMaxPermille: 700}}}
	item, ok := ApplyGuardrail(inv, rl, margin.Guard{MarginFloorPct: 5})
	if !ok {
		t.Fatal("expect ok=true")
	}
	if item.ParamRange.DiscountMinPermille != 500 {
		t.Errorf("MinPermille should be unchanged: got %d want 500", item.ParamRange.DiscountMinPermille)
	}
	if item.CostCeilCents != 1260 {
		t.Errorf("CostCeilCents: got %d want 1260", item.CostCeilCents)
	}
}

func TestApplyGuardrail_NoCost_Rejects(t *testing.T) {
	rl := Rule{ID: "r", Action: Action{ReasonType: ReasonClearanceCoupon, ParamRange: ParamRange{DiscountMinPermille: 500, DiscountMaxPermille: 700}}}
	// cost=0 → reject (no cost data, prevent hallucinated loss).
	if _, ok := ApplyGuardrail(signal.InventorySig{ProductID: 1, Name: "x", CostCents: 0, PriceCents: 1000}, rl, margin.Guard{MarginFloorPct: 5}); ok {
		t.Fatal("cost=0 must reject")
	}
	// price=0 → reject.
	if _, ok := ApplyGuardrail(signal.InventorySig{ProductID: 1, Name: "x", CostCents: 100, PriceCents: 0}, rl, margin.Guard{MarginFloorPct: 5}); ok {
		t.Fatal("price=0 must reject")
	}
}

func TestApplyGuardrail_Unsalvageable_Rejects(t *testing.T) {
	// rule max permille 450 < floor-respecting 490 → even least-aggressive breaches.
	inv := signal.InventorySig{ProductID: 1, Name: "葡萄", CostCents: 1200, PriceCents: 2580}
	rl := Rule{ID: "r", Action: Action{ReasonType: ReasonClearanceCoupon, ParamRange: ParamRange{DiscountMinPermille: 400, DiscountMaxPermille: 450}}}
	if _, ok := ApplyGuardrail(inv, rl, margin.Guard{MarginFloorPct: 5}); ok {
		t.Fatal("unsalvageable rule (max permille below floor) must reject")
	}
}

func TestApplyGuardrail_FloorDisabled_Passes(t *testing.T) {
	// floor=0 disables the check → deep discount allowed, costCeil = cost.
	inv := signal.InventorySig{ProductID: 1, Name: "x", CostCents: 1200, PriceCents: 2580}
	rl := Rule{ID: "r", Action: Action{ReasonType: ReasonClearanceCoupon, ParamRange: ParamRange{DiscountMinPermille: 300, DiscountMaxPermille: 500}}}
	item, ok := ApplyGuardrail(inv, rl, margin.Guard{MarginFloorPct: 0})
	if !ok {
		t.Fatal("floor=0 should pass")
	}
	if item.CostCeilCents != 1200 {
		t.Errorf("costCeil with floor=0: got %d want 1200 (=cost)", item.CostCeilCents)
	}
	if item.ParamRange.DiscountMinPermille != 300 {
		t.Errorf("no clamp expected: got %d want 300", item.ParamRange.DiscountMinPermille)
	}
}

// ===== Engine.Route: empty Bundle never errors =====

func TestRoute_EmptyBundle_NoError(t *testing.T) {
	eng := NewEngine(DefaultSeeds(), margin.Guard{MarginFloorPct: 5})

	// nil bundle → empty Routed, no error.
	routed, err := eng.Route(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil bundle err: %v", err)
	}
	if routed == nil || len(routed.Items) != 0 {
		t.Errorf("nil bundle: expect empty items, got %+v", routed)
	}

	// zero bundle (no matching conds) → empty items, no error (cold-start entry).
	b := &signal.Bundle{TenantID: 50021, BusinessType: "apparel"}
	routed2, err := eng.Route(context.Background(), b)
	if err != nil {
		t.Fatalf("zero bundle err: %v", err)
	}
	if len(routed2.Items) != 0 {
		t.Errorf("zero bundle: expect 0 items, got %d", len(routed2.Items))
	}
}

// ===== Multi-rule stacking + business="*" matching =====

func TestRoute_MultiRuleStack(t *testing.T) {
	// fruit + rain + an item nearing expiry → clearance AND rain both match
	// (rain_fruit is inventory-linked with no inv cond → emits per-item).
	eng := NewEngine(DefaultSeeds(), margin.Guard{MarginFloorPct: 5})
	b := &signal.Bundle{
		TenantID:     50021,
		BusinessType: "fruit",
		Weather:      signal.Weather{Condition: "rain"},
		Holiday:      signal.Holiday{Name: "无", DaysTo: -1},
		Inventory: []signal.InventorySig{
			{ProductID: 1, Name: "葡萄", CostCents: 1200, PriceCents: 2580, DaysToExpiry: 2},
		},
	}
	routed, _ := eng.Route(context.Background(), b)
	// Expect ≥2 items (clearance + rain); both ActGroupSeckill but distinct reasons.
	if len(routed.Items) < 2 {
		t.Fatalf("expect multi-rule stack, got %d item(s)", len(routed.Items))
	}
	reasons := map[string]bool{}
	for _, it := range routed.Items {
		reasons[it.ReasonType] = true
	}
	if !reasons[ReasonClearanceCoupon] || !reasons[ReasonRainDelivery] {
		t.Errorf("expected clearance+rain reasons, got %v", reasons)
	}
}

func TestRoute_DisabledRule_Skipped(t *testing.T) {
	// All seeds disabled → zero matches, no error.
	seeds := DefaultSeeds()
	for i := range seeds {
		seeds[i].Enable = false
	}
	eng := NewEngine(seeds, margin.Guard{MarginFloorPct: 5})
	b := &signal.Bundle{
		TenantID:     50021,
		BusinessType: "fruit",
		Inventory:    []signal.InventorySig{{ProductID: 1, CostCents: 1200, PriceCents: 2580, DaysToExpiry: 2}},
	}
	routed, err := eng.Route(context.Background(), b)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(routed.Items) != 0 {
		t.Errorf("disabled rules: expect 0 items, got %d", len(routed.Items))
	}
}
