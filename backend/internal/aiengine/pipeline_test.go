package aiengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/cache"
	"github.com/zhangguijunshi/backend/internal/aiengine/converge"
	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

func ctxWith(tenantID, storeID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID, StoreID: storeID, Source: multitenancy.SourceMerchant,
	})
}

// === fakes ===

type fakeCollector struct {
	bundle *signal.Bundle
	err    error
	delay  time.Duration // blocks this long, ctx-aware (timeout test)
}

func (f *fakeCollector) Collect(ctx context.Context, _, _ int64) (*signal.Bundle, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.bundle, nil
}

type fakeRouter struct {
	routed *rule.Routed
}

func (f *fakeRouter) Route(_ context.Context, b *signal.Bundle) (*rule.Routed, error) {
	if f.routed != nil {
		return f.routed, nil
	}
	// Default: real-ish empty routed carrying the bundle tenant.
	tid := int64(0)
	if b != nil {
		tid = b.TenantID
	}
	return &rule.Routed{TenantID: tid, Items: nil}, nil
}

type fakeProvider struct {
	name    string
	content string
	err     error
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) Chat(_ context.Context, _ []gateway.Message, _ gateway.Opts) (gateway.Resp, error) {
	if f.err != nil {
		return gateway.Resp{}, f.err
	}
	return gateway.Resp{Content: f.content, CostCNY: 0.0004}, nil
}

// buildPipeline assembles a Pipeline with a memory strategy repo + memory cache
// and the given collector/router/stage. The converge / output / cache are real.
func buildPipeline(t *testing.T, col signal.Collector, router rule.Engine, stage agent.Stage, opts ...Option) (*Pipeline, *strategy.Service, *cache.MemoryCache) {
	t.Helper()
	repo := strategy.NewMemoryStrategyRepo()
	svc := strategy.New(repo, strategy.WithNowFunc(func() time.Time {
		return time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	}))
	enc := output.NewEncoder(svc)
	ch := cache.NewMemoryCache()
	allOpts := append([]Option{WithNowFunc(func() time.Time {
		return time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	})}, opts...)
	p := NewPipeline(col, router, stage, converge.NewTop3(), enc, ch, allOpts...)
	return p, svc, ch
}

const goodLLMJSON = `{"copy":"临期芒果群秒杀 6折速抢","params":{"discount_permille":600,"limit_qty":50,"expire_hours":24},"expected_effect":"核销35份 挽回损耗¥180"}`

func gwWithGoodJSON(t *testing.T) *gateway.Gateway {
	t.Helper()
	gw, err := gateway.New(
		[]gateway.Provider{&fakeProvider{name: "fake", content: goodLLMJSON}},
		gateway.Route{Provider: "fake"},
		gateway.WithBudget(gateway.NewMemoryBudget(2.0)),
	)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return gw
}

// oneRoutedItem is a valid group_seckill routed item the fake LLM polishes.
func oneRoutedItem(tenantID int64) *rule.Routed {
	return &rule.Routed{
		TenantID: tenantID,
		Items: []rule.RoutedItem{{
			RuleID:         "clearance_fruit_3d",
			ReasonType:     rule.ReasonClearanceCoupon,
			ActionType:     rule.ActGroupSeckill,
			Target:         "临期SKU群秒杀",
			ProductID:      42,
			Product:        "芒果",
			CostCents:      1200,
			PriceCents:     2580,
			CostCeilCents:  1260,
			MarginFloorPct: 5,
			Priority:       50,
			ParamRange: rule.ParamRange{
				DiscountMinPermille: 500,
				DiscountMaxPermille: 700,
				LimitQtyMax:         50,
				ExpireHours:         24,
			},
		}},
	}
}

// === tests ===

// TestPipeline_Happy_LiveLLM: real-ish flow, fake LLM returns good JSON →
// Degrade empty, GeneratedBy=live_llm, one item with LLM-polished copy.
func TestPipeline_Happy_LiveLLM(t *testing.T) {
	bundle := &signal.Bundle{TenantID: 7001, StoreID: 7100, BusinessType: "fruit", DataQuality: 70,
		Inventory: []signal.InventorySig{{ProductID: 42, Name: "芒果", CostCents: 1200, PriceCents: 2580, DaysToExpiry: 2}}}
	p, svc, _ := buildPipeline(t,
		&fakeCollector{bundle: bundle},
		&fakeRouter{routed: oneRoutedItem(7001)},
		agent.New(gwWithGoodJSON(t)),
	)

	advice, err := p.Run(ctxWith(7001, 7100))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advice.Degrade != "" {
		t.Errorf("Degrade = %q, want empty (live_llm)", advice.Degrade)
	}
	if advice.Strategy.GeneratedBy != strategy.GenLiveLLM {
		t.Errorf("GeneratedBy = %q, want live_llm", advice.Strategy.GeneratedBy)
	}
	if len(advice.Strategy.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(advice.Strategy.Items))
	}
	// Persisted.
	if got, err := svc.GetToday(ctxWith(7001, 7100), 7100, "2026-07-28"); err != nil || got.GeneratedBy != strategy.GenLiveLLM {
		t.Errorf("GetToday after Run: %+v err=%v", got, err)
	}
}

// TestPipeline_CacheShortCircuit: a pre-populated cache short-circuits the
// pipeline at stage 0 with Degrade=cache_hit.
func TestPipeline_CacheShortCircuit(t *testing.T) {
	p, _, ch := buildPipeline(t,
		&fakeCollector{err: errors.New("should not be called")},
		&fakeRouter{},
		agent.New(nil),
	)
	cached := &strategy.Strategy{TenantID: 7001, StoreID: 7100, ForDate: "2026-07-28", GeneratedBy: strategy.GenCacheHit, Status: strategy.StatusGenerated}
	ch.Put(ctxWith(7001, 7100), 7001, 7100, "2026-07-28", cached)

	advice, err := p.Run(ctxWith(7001, 7100))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advice.Degrade != "cache_hit" {
		t.Errorf("Degrade = %q, want cache_hit", advice.Degrade)
	}
	if advice.Strategy == nil || advice.Strategy.ForDate != "2026-07-28" {
		t.Errorf("cache hit returned wrong strategy: %+v", advice.Strategy)
	}
}

// TestPipeline_SignalFail_RuleOnly: collector error → empty bundle → cold-start
// injection → GeneratedBy=rule_only, no Run error.
func TestPipeline_SignalFail_RuleOnly(t *testing.T) {
	p, svc, _ := buildPipeline(t,
		&fakeCollector{err: errors.New("lookup down")},
		&fakeRouter{},
		agent.New(nil),
	)

	advice, err := p.Run(ctxWith(7001, 7100))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advice == nil || advice.Strategy == nil {
		t.Fatal("nil advice")
	}
	if advice.Strategy.GeneratedBy != strategy.GenRuleOnly {
		t.Errorf("GeneratedBy = %q, want rule_only", advice.Strategy.GeneratedBy)
	}
	// Persisted as rule_only.
	got, err := svc.GetToday(ctxWith(7001, 7100), 7100, "2026-07-28")
	if err != nil {
		t.Fatalf("GetToday: %v", err)
	}
	if got.GeneratedBy != strategy.GenRuleOnly {
		t.Errorf("persisted GeneratedBy = %q", got.GeneratedBy)
	}
}

// TestPipeline_ColdStart: route returns 0 items → ≥1 cold-start item injected,
// Degrade=cold_start, action_type=coupon_wake.
func TestPipeline_ColdStart(t *testing.T) {
	bundle := &signal.Bundle{TenantID: 7001, StoreID: 7100, BusinessType: "fruit"} // no triggering inventory
	p, _, _ := buildPipeline(t,
		&fakeCollector{bundle: bundle},
		&fakeRouter{}, // empty routed
		agent.New(nil),
	)

	advice, err := p.Run(ctxWith(7001, 7100))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advice.Degrade != "cold_start" {
		t.Errorf("Degrade = %q, want cold_start", advice.Degrade)
	}
	if len(advice.Strategy.Items) < 1 {
		t.Fatalf("cold-start must inject ≥1 item, got %d", len(advice.Strategy.Items))
	}
	for _, it := range advice.Strategy.Items {
		if it.ActionType != rule.ActCouponWake {
			t.Errorf("cold-start item ActionType = %q, want coupon_wake", it.ActionType)
		}
		if it.ReasonType != rule.ReasonWakeDormant && it.ReasonType != rule.ReasonFestival {
			t.Errorf("cold-start item ReasonType = %q", it.ReasonType)
		}
	}
}

// TestPipeline_TimeoutDegrades: an injected short timeout + blocking collector
// must NOT crash the pipeline; it degrades to a rule_only advice.
func TestPipeline_TimeoutDegrades(t *testing.T) {
	p, _, _ := buildPipeline(t,
		&fakeCollector{delay: 400 * time.Millisecond}, // longer than the 50ms timeout
		&fakeRouter{},
		agent.New(nil),
		WithTimeout(50*time.Millisecond),
	)

	advice, err := p.Run(ctxWith(7001, 7100))
	if err != nil {
		t.Fatalf("Run must not hard-error on timeout: %v", err)
	}
	if advice == nil || advice.Strategy == nil {
		t.Fatal("nil advice on timeout")
	}
	// Timeout → collect fails → cold-start → rule_only.
	if advice.Strategy.GeneratedBy != strategy.GenRuleOnly {
		t.Errorf("GeneratedBy = %q, want rule_only after timeout", advice.Strategy.GeneratedBy)
	}
}

// TestPipeline_NoProvider_RuleOnly: a no-provider gateway (dev without keys)
// drives EnhanceOrSkip to no_provider; degrade is no_provider, GeneratedBy
// rule_only, but the routed item still surfaces as a template item.
func TestPipeline_NoProvider_RuleOnly(t *testing.T) {
	bundle := &signal.Bundle{TenantID: 7001, StoreID: 7100, BusinessType: "fruit", DataQuality: 40}
	// no-provider gateway: NewFromConfig with empty config → Chat returns ErrNoProvider.
	noProvGW := func(t *testing.T) *gateway.Gateway {
		t.Helper()
		// Build a gateway with one real provider name but no provider registered is
		// impossible via New(); emulate no-provider by passing nil to agent.Stage,
		// which classifies as no_provider (CONTRACT §12.5.10).
		return nil
	}
	p, _, _ := buildPipeline(t,
		&fakeCollector{bundle: bundle},
		&fakeRouter{routed: oneRoutedItem(7001)},
		agent.New(noProvGW(t)),
	)

	advice, err := p.Run(ctxWith(7001, 7100))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advice.Degrade != "no_provider" {
		t.Errorf("Degrade = %q, want no_provider", advice.Degrade)
	}
	if advice.Strategy.GeneratedBy != strategy.GenRuleOnly {
		t.Errorf("GeneratedBy = %q, want rule_only", advice.Strategy.GeneratedBy)
	}
	if len(advice.Strategy.Items) != 1 {
		t.Fatalf("template item must still surface, got %d items", len(advice.Strategy.Items))
	}
}

// TestPipeline_CachePutAfterRun: a successful Run writes the advice into the
// cache so the next Run short-circuits.
func TestPipeline_CachePutAfterRun(t *testing.T) {
	bundle := &signal.Bundle{TenantID: 7001, StoreID: 7100, BusinessType: "fruit", DataQuality: 70}
	p, _, ch := buildPipeline(t,
		&fakeCollector{bundle: bundle},
		&fakeRouter{routed: oneRoutedItem(7001)},
		agent.New(gwWithGoodJSON(t)),
	)

	if _, err := p.Run(ctxWith(7001, 7100)); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, ok := ch.TryHit(ctxWith(7001, 7100), 7001, 7100, "2026-07-28"); !ok {
		t.Errorf("cache not populated after Run")
	}
}

// TestPipeline_NoTenant: a context without a tenant errors immediately.
func TestPipeline_NoTenant(t *testing.T) {
	p, _, _ := buildPipeline(t, &fakeCollector{}, &fakeRouter{}, agent.New(nil))
	if _, err := p.Run(context.Background()); err == nil {
		t.Errorf("Run without tenant must error")
	}
}
