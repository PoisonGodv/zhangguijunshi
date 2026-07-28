package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// fakeProvider is a controllable gateway.Provider for agent tests. content is
// returned as Resp.Content; err is returned instead when non-nil.
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

// goodLLMJSON is a well-formed polish response for a group_seckill item.
const goodLLMJSON = `{"copy":"临期芒果群秒杀 6折速抢","params":{"discount_permille":600,"limit_qty":50,"expire_hours":24},"expected_effect":"核销35份 挽回损耗¥180"}`

func sampleRouted() *rule.Routed {
	return &rule.Routed{
		TenantID: 7001,
		Items: []rule.RoutedItem{{
			RuleID:         "clearance_fruit_3d",
			ReasonType:     rule.ReasonClearanceCoupon,
			ActionType:     rule.ActGroupSeckill,
			Target:         "临期SKU群秒杀",
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

// gatewayWith builds a real *gateway.Gateway served by one fake provider.
func gatewayWith(t *testing.T, p *fakeProvider) *gateway.Gateway {
	t.Helper()
	gw, err := gateway.New([]gateway.Provider{p}, gateway.Route{Provider: p.Name()}, gateway.WithBudget(gateway.NewMemoryBudget(2.0)))
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return gw
}

// noProviderGateway builds a placeholder gateway with no providers so Chat
// returns gateway.ErrNoProvider (CONTRACT §12.5.10).
func noProviderGateway(t *testing.T) *gateway.Gateway {
	t.Helper()
	gw, err := gateway.NewFromConfig(config.AIConfig{})
	if err != nil {
		t.Fatalf("NewFromConfig: %v", err)
	}
	return gw
}

// exceededBudgetGateway builds a gateway whose budget is already over the cap so
// Chat returns gateway.ErrBudgetExceeded.
func exceededBudgetGateway(t *testing.T, p *fakeProvider, tenantID int64) *gateway.Gateway {
	t.Helper()
	budget := gateway.NewMemoryBudget(2.0)
	if err := budget.AddCost(context.Background(), tenantID, 9.0); err != nil {
		t.Fatalf("AddCost: %v", err)
	}
	gw, err := gateway.New([]gateway.Provider{p}, gateway.Route{Provider: p.Name()}, gateway.WithBudget(budget))
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return gw
}

// TestEnhanceOrSkip_NeverErrors covers the four CONTRACT §12.6 scenarios. Every
// path must return a non-nil *Enhanced, a non-empty Items slice, and no error.
func TestEnhanceOrSkip_NeverErrors(t *testing.T) {
	r := sampleRouted()
	degrade := ""

	cases := []struct {
		name    string
		gw      *gateway.Gateway
		wantHit bool
		wantDeg string
	}{
		{
			name:    "good JSON",
			gw:      gatewayWith(t, &fakeProvider{name: "fake", content: goodLLMJSON}),
			wantHit: true,
			wantDeg: "",
		},
		{
			name:    "bad JSON",
			gw:      gatewayWith(t, &fakeProvider{name: "fake", content: "not json at all"}),
			wantHit: false,
			wantDeg: "llm",
		},
		{
			name:    "budget exceeded",
			gw:      exceededBudgetGateway(t, &fakeProvider{name: "fake"}, 7001),
			wantHit: false,
			wantDeg: "budget",
		},
		{
			name:    "no provider",
			gw:      noProviderGateway(t),
			wantHit: false,
			wantDeg: "no_provider",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			degrade = ""
			svc := New(tc.gw)
			got := svc.EnhanceOrSkip(context.Background(), 7001, r, &degrade)

			if got == nil {
				t.Fatal("EnhanceOrSkip returned nil")
			}
			if len(got.Items) != 1 {
				t.Fatalf("expected 1 item, got %d", len(got.Items))
			}
			it := got.Items[0]
			if !it.MarginOK {
				t.Errorf("MarginOK must be true even on degrade (got false)")
			}
			if it.Copy == "" {
				t.Errorf("Copy must be non-empty (template or LLM)")
			}
			if tc.wantDeg != degrade {
				t.Errorf("degrade = %q, want %q", degrade, tc.wantDeg)
			}
			if tc.wantHit {
				// LLM-produced copy differs from the template; the good JSON
				// copy mentions the product "芒果".
				if !strings.Contains(it.Copy, "芒果") && it.ExpectedEffect == "" {
					t.Errorf("expected LLM-polished item, got copy=%q effect=%q", it.Copy, it.ExpectedEffect)
				}
				if it.ExpectedEffect == "" {
					t.Errorf("LLM hit should carry expected_effect")
				}
			}
		})
	}
}

// TestEnhanceOrSkip_NilGateway: a nil gateway (no provider) degrades to
// template items with degrade=no_provider and still returns non-nil.
func TestEnhanceOrSkip_NilGateway(t *testing.T) {
	degrade := ""
	svc := New(nil)
	got := svc.EnhanceOrSkip(context.Background(), 7001, sampleRouted(), &degrade)
	if got == nil || len(got.Items) != 1 {
		t.Fatalf("expected 1 non-nil item, got %+v", got)
	}
	if got.Items[0].Copy == "" {
		t.Errorf("template copy must be non-empty")
	}
	if degrade != "no_provider" {
		t.Errorf("degrade = %q, want no_provider", degrade)
	}
}

// TestEnhanceOrSkip_PreservesColdStart: when *degrade is already "cold_start"
// (set by an upstream stage), a full LLM miss must NOT overwrite it.
func TestEnhanceOrSkip_PreservesColdStart(t *testing.T) {
	degrade := "cold_start"
	svc := New(nil)
	svc.EnhanceOrSkip(context.Background(), 7001, sampleRouted(), &degrade)
	if degrade != "cold_start" {
		t.Errorf("degrade overwritten to %q; cold_start must be preserved", degrade)
	}
}

// TestParseLLMJSON_FloorClamp: an LLM that undercuts the floor permille is
// clamped back so margin.ValidatePriceAdj passes.
func TestParseLLMJSON_FloorClamp(t *testing.T) {
	ri := sampleRouted().Items[0]
	// 300 permille (3折) on a 5% floor product would breach; clamp must lift it
	// to the rule's DiscountMinPermille (500 = 5折) which rule.ApplyGuardrail
	// already proved floor-safe.
	ei, ok := parseLLMJSON(`{"copy":"x","params":{"discount_permille":300},"expected_effect":""}`, ri)
	if !ok {
		t.Fatal("expected parse ok")
	}
	perm, _ := ei.Params["discount_permille"].(int64)
	if perm != 500 {
		t.Errorf("discount_permille = %d, want 500 (floor clamp)", perm)
	}
	if !ei.MarginOK {
		t.Errorf("MarginOK must be true after clamp")
	}
}

// TestClassifyChatErr covers the three error-classification branches.
func TestClassifyChatErr(t *testing.T) {
	if got := classifyChatErr(gateway.ErrNoProvider); got != "no_provider" {
		t.Errorf("ErrNoProvider -> %q", got)
	}
	if got := classifyChatErr(gateway.ErrBudgetExceeded); got != "budget" {
		t.Errorf("ErrBudgetExceeded -> %q", got)
	}
	if got := classifyChatErr(gateway.ErrUpstreamTimeout); got != "llm" {
		t.Errorf("ErrUpstreamTimeout -> %q", got)
	}
	if got := classifyChatErr(errors.New("boom")); got != "llm" {
		t.Errorf("generic err -> %q", got)
	}
}
