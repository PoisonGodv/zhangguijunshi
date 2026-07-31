package output

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/compliance/contentfilter"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

func ctxWith(tenantID, storeID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID, StoreID: storeID, Source: multitenancy.SourceMerchant,
	})
}

func top3Items() []*agent.EnhancedItem {
	return []*agent.EnhancedItem{{
		Routed: rule.RoutedItem{
			RuleID:         "clearance_fruit_3d",
			ReasonType:     rule.ReasonClearanceCoupon,
			ActionType:     rule.ActGroupSeckill,
			Target:         "临期SKU群秒杀",
			ProductID:      42,
			Product:        "芒果",
			Priority:       50,
			CostCents:      1200,
			PriceCents:     2580,
			MarginFloorPct: 5,
		},
		Copy:           "临期芒果群秒杀 6折速抢",
		Params:         map[string]any{"discount_permille": int64(600), "limit_qty": 50},
		ExpectedEffect: "核销35份 挽回损耗¥180",
		MarginOK:       true,
	}}
}

func sampleBundle() *signal.Bundle {
	return &signal.Bundle{
		TenantID:     7001,
		StoreID:      7100,
		BusinessType: "fruit",
		DataQuality:  70,
		Inventory:    []signal.InventorySig{{ProductID: 42, Name: "芒果"}},
	}
}

// TestEncode_PersistsAndMapsFields: a happy Encode writes one strategy row with
// ActionType/ReasonType on the item, copy≤40字, GeneratedBy=live_llm (degrade
// empty), and a non-empty Signals snapshot.
func TestEncode_PersistsAndMapsFields(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time {
		return time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	}))
	enc := NewEncoder(svc)

	advice, err := enc.Encode(ctxWith(7001, 7100), 7001, 7100, top3Items(), sampleBundle(), "")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if advice == nil || advice.Strategy == nil {
		t.Fatal("nil advice/strategy")
	}
	if advice.Degrade != "" {
		t.Errorf("Degrade = %q, want empty", advice.Degrade)
	}
	st := advice.Strategy
	if st.GeneratedBy != strategy.GenLiveLLM {
		t.Errorf("GeneratedBy = %q, want live_llm", st.GeneratedBy)
	}
	if st.LLMTier != "lite" {
		t.Errorf("LLMTier = %q, want lite", st.LLMTier)
	}
	if st.ForDate != today {
		t.Errorf("ForDate = %q", st.ForDate)
	}
	if len(st.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(st.Items))
	}
	it := st.Items[0]
	if it.ActionType != rule.ActGroupSeckill {
		t.Errorf("ActionType = %q", it.ActionType)
	}
	if it.ReasonType != rule.ReasonClearanceCoupon {
		t.Errorf("ReasonType = %q", it.ReasonType)
	}
	if it.Rank != 1 {
		t.Errorf("Rank = %d", it.Rank)
	}
	if !it.MarginOK {
		t.Errorf("MarginOK forced true")
	}
	if it.ExecutionStatus != strategy.ItemPending {
		t.Errorf("ExecutionStatus = %q", it.ExecutionStatus)
	}
	if runeLen(it.Copy) > copyMaxRunes {
		t.Errorf("Copy len = %d runes, want ≤%d", runeLen(it.Copy), copyMaxRunes)
	}
	if len(st.Signals) == 0 {
		t.Errorf("Signals snapshot must be non-empty")
	}
	if !strings.Contains(string(st.Signals), "fruit") {
		t.Errorf("Signals snapshot missing business_type: %s", st.Signals)
	}
	if it.ProductID != 42 {
		t.Errorf("ProductID = %d, want 42", it.ProductID)
	}

	// Persisted: a fresh load via GetToday returns the same row+item.
	got, err := svc.GetToday(ctxWith(7001, 7100), 7100, today)
	if err != nil {
		t.Fatalf("GetToday: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].ActionType != rule.ActGroupSeckill {
		t.Errorf("repo round-trip lost item: %+v", got)
	}
}

// TestEncode_DegradeDerivesGeneratedBy: each degrade marker maps to rule_only.
func TestEncode_DegradeDerivesGeneratedBy(t *testing.T) {
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time { return time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC) }))
	enc := NewEncoder(svc)

	for _, deg := range []string{"cold_start", "budget", "no_provider", "llm"} {
		advice, err := enc.Encode(ctxWith(7001, 7100), 7001, 7100, top3Items(), sampleBundle(), deg)
		if err != nil {
			t.Fatalf("Encode(%s): %v", deg, err)
		}
		if advice.Strategy.GeneratedBy != strategy.GenRuleOnly {
			t.Errorf("degrade=%q GeneratedBy = %q, want rule_only", deg, advice.Strategy.GeneratedBy)
		}
		if advice.Strategy.LLMTier != "" {
			t.Errorf("degrade=%q LLMTier = %q, want empty", deg, advice.Strategy.LLMTier)
		}
		if advice.Degrade != deg {
			t.Errorf("Degrade = %q, want %q", advice.Degrade, deg)
		}
	}
}

// TestEncode_CopyFilterFallback: a copy containing a sensitive word (DFA hard
// block) falls back to the agent template for the ActionType.
func TestEncode_CopyFilterFallback(t *testing.T) {
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time { return time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC) }))
	enc := NewEncoder(svc, WithFilter(contentfilter.New(
		contentfilter.WithSensitiveWords([]string{"违禁词"}),
	)))

	items := []*agent.EnhancedItem{{
		Routed:         rule.RoutedItem{ActionType: rule.ActCouponWake, ReasonType: rule.ReasonWakeDormant, Target: "member_segment"},
		Copy:           "含违禁词的文案应被拦截",
		Params:         map[string]any{},
		ExpectedEffect: "召回4人",
		MarginOK:       true,
	}}
	advice, err := enc.Encode(ctxWith(7001, 7100), 7001, 7100, items, sampleBundle(), "llm")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got := advice.Strategy.Items[0].Copy
	want := agent.TemplateCopy(rule.ActCouponWake)
	if got != want {
		t.Errorf("Copy = %q, want template %q (sensitive fallback)", got, want)
	}
}

// TestEncode_AbsolutismReplacement: an absolutism word is replaced (soft) and
// the copy stays usable, within the rune cap.
func TestEncode_AbsolutismReplacement(t *testing.T) {
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time { return time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC) }))
	enc := NewEncoder(svc)

	items := []*agent.EnhancedItem{{
		Routed:         rule.RoutedItem{ActionType: rule.ActGroupSeckill, ReasonType: rule.ReasonClearanceCoupon, Target: "群"},
		Copy:           "全网最佳芒果秒杀",
		Params:         map[string]any{},
		ExpectedEffect: "核销10份",
		MarginOK:       true,
	}}
	advice, err := enc.Encode(ctxWith(7001, 7100), 7001, 7100, items, sampleBundle(), "")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got := advice.Strategy.Items[0].Copy
	if strings.Contains(got, "最佳") {
		t.Errorf("absolutism word 最佳 must be replaced: %q", got)
	}
	if got == "" || runeLen(got) > copyMaxRunes {
		t.Errorf("bad copy: %q", got)
	}
}

// TestEncode_NilBundleAndItems: nil bundle + empty top3 still persists a valid
// (zero-item) strategy without error.
func TestEncode_NilBundleAndItems(t *testing.T) {
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time { return time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC) }))
	enc := NewEncoder(svc)

	advice, err := enc.Encode(ctxWith(7001, 7100), 7001, 7100, nil, nil, "cold_start")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(advice.Strategy.Items) != 0 {
		t.Errorf("expected 0 items, got %d", len(advice.Strategy.Items))
	}
	if advice.Strategy.Signals != nil {
		t.Errorf("nil bundle -> nil Signals, got %s", advice.Strategy.Signals)
	}
	if advice.Strategy.GeneratedBy != strategy.GenRuleOnly {
		t.Errorf("cold_start -> GeneratedBy = %q", advice.Strategy.GeneratedBy)
	}
}

// TestEncode_CrossTenantIsolation: the strategy service rejects a context
// without a tenant; a different tenant cannot read another's advice.
func TestEncode_CrossTenantIsolation(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time { return time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC) }))
	enc := NewEncoder(svc)

	if _, err := enc.Encode(context.Background(), 7001, 7100, top3Items(), sampleBundle(), ""); err == nil {
		t.Errorf("Encode without tenant must error")
	}
	// Persist as 7001.
	if _, err := enc.Encode(ctxWith(7001, 7100), 7001, 7100, top3Items(), sampleBundle(), ""); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Tenant 7002 cannot see it.
	if _, err := svc.GetToday(ctxWith(7002, 7100), 7100, today); err != strategy.ErrNotFound {
		t.Errorf("cross-tenant GetToday err = %v, want ErrNotFound", err)
	}
}

func runeLen(s string) int { return len([]rune(s)) }

// TestEncode_LiftsCostPriceOntoParams (aiengine 修复②): for a product-bound
// item, Encode must write the RoutedItem's cost_cents / price_cents into Params
// so the executor's third margin defense-in-depth check has a real view to
// validate. Without this, marginOkForCoupon always saw an empty view and
// silently deferred — the margin recheck never actually ran.
func TestEncode_LiftsCostPriceOntoParams(t *testing.T) {
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time { return time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC) }))
	enc := NewEncoder(svc)

	advice, err := enc.Encode(ctxWith(7001, 7100), 7001, 7100, top3Items(), sampleBundle(), "")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	params := advice.Strategy.Items[0].Params
	if got := numParam(params, "cost_cents"); got != 1200 {
		t.Errorf("cost_cents = %d, want 1200 (lifted from Routed.CostCents)", got)
	}
	if got := numParam(params, "price_cents"); got != 2580 {
		t.Errorf("price_cents = %d, want 2580 (lifted from Routed.PriceCents)", got)
	}
}

// TestEncode_NonProductItemNoCostPrice: a full-reduction (non-product) action
// carries CostCents/PriceCents == 0, so Encode must NOT inject cost/price keys
// (its margin pass stays trivially satisfied).
func TestEncode_NonProductItemNoCostPrice(t *testing.T) {
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time { return time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC) }))
	enc := NewEncoder(svc)

	items := []*agent.EnhancedItem{{
		Routed:   rule.RoutedItem{ActionType: rule.ActCouponWake, ReasonType: rule.ReasonWakeDormant, Target: "member_segment"},
		Copy:     "沉睡唤醒券",
		Params:   map[string]any{"threshold_cents": int64(3000), "off_cents": int64(500)},
		MarginOK: true,
	}}
	advice, err := enc.Encode(ctxWith(7001, 7100), 7001, 7100, items, sampleBundle(), "")
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	params := advice.Strategy.Items[0].Params
	if _, ok := params["cost_cents"]; ok {
		t.Errorf("non-product item must not get cost_cents: %+v", params)
	}
	if _, ok := params["price_cents"]; ok {
		t.Errorf("non-product item must not get price_cents: %+v", params)
	}
}

func numParam(m map[string]any, key string) int64 {
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
