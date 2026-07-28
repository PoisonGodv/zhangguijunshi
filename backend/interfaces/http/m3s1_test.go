package httpserver

// M3-S1 HTTP-layer tests (CONTRACT §12.6): the AI engine boss loop — generate
// today's advice (with degrade paths), one-click execute (executed / blocked
// attribution), and cross-tenant isolation. The pipeline is wired with a fake
// signal.Lookuper + a fake gateway.Provider so the degrade paths (no_provider /
// budget / llm-bad-json / cold_start) are deterministic; the executor is wired
// with fake anti-corruption ports so blocked attribution is asserted over HTTP.

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/aiengine"
	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/cache"
	"github.com/zhangguijunshi/backend/internal/aiengine/converge"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// --- fakes ---

type m3s1Lookuper struct {
	inv []signal.InventorySig
	mem signal.MemberStats
	txn signal.TxnSig
}

func (l m3s1Lookuper) Inventory(_ context.Context, _, _ int64) ([]signal.InventorySig, error) {
	return l.inv, nil
}
func (l m3s1Lookuper) Members(_ context.Context, _ int64) (signal.MemberStats, error) {
	return l.mem, nil
}
func (l m3s1Lookuper) DailyTxn(_ context.Context, _, _ int64, _ string) (signal.TxnSig, error) {
	return l.txn, nil
}

type m3s1Provider struct {
	name string
	resp gateway.Resp
	err  error
}

func (p *m3s1Provider) Name() string { return p.name }
func (p *m3s1Provider) Chat(_ context.Context, _ []gateway.Message, _ gateway.Opts) (gateway.Resp, error) {
	return p.resp, p.err
}

type m3s1Coupons struct{ err error }

func (c *m3s1Coupons) Issue(_ context.Context, memberID, _, _ int64, _ *time.Time) (*exec.CouponRef, error) {
	if c.err != nil {
		return nil, c.err
	}
	return &exec.CouponRef{MemberCouponID: 9001, MemberID: memberID}, nil
}

type m3s1Sender struct {
	ref *exec.SendRef
	err error
}

func (s *m3s1Sender) Send(_ context.Context, _, _, _ int64, _, _ string, _ map[string]string) (*exec.SendRef, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.ref != nil {
		return s.ref, nil
	}
	return &exec.SendRef{TaskID: 7001, Status: "sent"}, nil
}

type m3s1Pricer struct{ err error }

func (p *m3s1Pricer) UpdatePrice(_ context.Context, _, _, _ int64) error { return p.err }

// --- router + seed helpers ---

// newM3S1Router builds a router with the AI engine wired over fake ports +
// in-memory strategy repo. prov nil → empty-provider gateway (ErrNoProvider →
// no_provider degrade). budget nil → default MemoryBudget.
func newM3S1Router(lk signal.Lookuper, prov gateway.Provider, budget gateway.Budget, coupons exec.CouponIssuer, sender exec.MessageSender, pricer exec.PriceUpdater) (*gin.Engine, *strategy.Service) {
	strategySvc := strategy.New(strategy.NewMemoryStrategyRepo())
	guard := margin.Guard{MarginFloorPct: 5}
	collector := signal.NewCollector("fruit", "", lk)
	ruleEngine := rule.NewEngine(rule.DefaultSeeds(), guard)
	var gw *gateway.Gateway
	if prov != nil {
		opts := []gateway.Option{}
		if budget != nil {
			opts = append(opts, gateway.WithBudget(budget))
		}
		gw, _ = gateway.New([]gateway.Provider{prov}, gateway.Route{Provider: prov.Name()}, opts...)
	} else {
		gw, _ = gateway.NewFromConfig(config.AIConfig{})
	}
	pipeline := aiengine.NewPipeline(
		collector, ruleEngine, agent.New(gw), converge.NewTop3(),
		output.NewEncoder(strategySvc), cache.NewMemoryCache(),
	)
	executor := exec.NewExecutor(strategySvc, guard, coupons, sender, pricer, nil)
	return New(Deps{
		Cfg:      &config.Config{App: config.AppConfig{Env: "test"}},
		Strategy: strategySvc,
		Pipeline: pipeline,
		Executor: executor,
		DevMock:  true,
	}), strategySvc
}

// nearingApple is a signal.InventorySig that triggers the fruit clearance rule.
func nearingApple() signal.InventorySig {
	return signal.InventorySig{ProductID: 1, Name: "苹果", CostCents: 1200, PriceCents: 2580, DaysToExpiry: 2}
}

// seedItem persists one strategy + item under (tid,sid) and returns (strategyID, itemID).
func seedItem(t *testing.T, svc *strategy.Service, tid, sid int64, it strategy.StrategyItem) (int64, int64) {
	t.Helper()
	ctx := multitenancy.WithContext(context.Background(), multitenancy.Context{TenantID: tid, StoreID: sid})
	s, err := svc.SaveAdvice(ctx, &strategy.Strategy{
		TenantID: tid, StoreID: sid, ForDate: "2026-07-28",
		GeneratedBy: strategy.GenRuleOnly, Status: strategy.StatusGenerated,
		Items: []strategy.StrategyItem{it},
	})
	require.NoError(t, err)
	return s.ID, s.Items[0].ID
}

func todayAdvice(t *testing.T, r *gin.Engine, tenant, store string) (int, map[string]any) {
	t.Helper()
	env, data := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", tenant, store, gin.H{})
	return env.Code, data
}

func itemsOf(data map[string]any) []any {
	strat, _ := data["strategy"].(map[string]any)
	if strat == nil {
		return nil
	}
	items, _ := strat["items"].([]any)
	return items
}

// --- tests ---

// TestM3S1_Today_NoProvider_RuleOnly: signal present + no LLM provider →
// rule matches → advice ≥1 item, degrade=no_provider.
func TestM3S1_Today_NoProvider_RuleOnly(t *testing.T) {
	r, _ := newM3S1Router(m3s1Lookuper{inv: []signal.InventorySig{nearingApple()}}, nil, nil, nil, nil, nil)
	code, data := todayAdvice(t, r, "1", "10")
	require.Equal(t, 0, code)
	require.Equal(t, "no_provider", data["degrade"])
	require.GreaterOrEqual(t, len(itemsOf(data)), 1)
}

// TestM3S1_Today_ColdStart: empty signals → 0 rule hits → cold-start injection
// → degrade=cold_start, ≥1 item.
func TestM3S1_Today_ColdStart(t *testing.T) {
	r, _ := newM3S1Router(m3s1Lookuper{}, nil, nil, nil, nil, nil)
	code, data := todayAdvice(t, r, "1", "10")
	require.Equal(t, 0, code)
	require.Equal(t, "cold_start", data["degrade"])
	require.GreaterOrEqual(t, len(itemsOf(data)), 1)
}

// TestM3S1_Today_BudgetExceeded: tenant over the monthly ceiling → degrade=budget.
func TestM3S1_Today_BudgetExceeded(t *testing.T) {
	prov := &m3s1Provider{name: "fake", resp: gateway.Resp{Content: `{"copy":"x","params":{}}`}}
	budget := gateway.NewMemoryBudget(2.0)
	require.NoError(t, budget.AddCost(context.Background(), 1, 3.0)) // over the ¥2 ceiling
	r, _ := newM3S1Router(m3s1Lookuper{inv: []signal.InventorySig{nearingApple()}}, prov, budget, nil, nil, nil)
	code, data := todayAdvice(t, r, "1", "10")
	require.Equal(t, 0, code)
	require.Equal(t, "budget", data["degrade"])
}

// TestM3S1_Today_LLMBadJSON: provider returns non-JSON → degrade=llm, advice
// still comes from rules.
func TestM3S1_Today_LLMBadJSON(t *testing.T) {
	prov := &m3s1Provider{name: "fake", resp: gateway.Resp{Content: "not json at all"}}
	r, _ := newM3S1Router(m3s1Lookuper{inv: []signal.InventorySig{nearingApple()}}, prov, nil, nil, nil, nil)
	code, data := todayAdvice(t, r, "1", "10")
	require.Equal(t, 0, code)
	require.Equal(t, "llm", data["degrade"])
	require.GreaterOrEqual(t, len(itemsOf(data)), 1)
}

// TestM3S1_Execute_CouponWake_Executed: issue coupon + reach sent → executed.
func TestM3S1_Execute_CouponWake_Executed(t *testing.T) {
	r, svc := newM3S1Router(m3s1Lookuper{}, nil, nil, &m3s1Coupons{}, &m3s1Sender{}, nil)
	_, itemID := seedItem(t, svc, 1, 10, strategy.StrategyItem{
		Rank: 1, ActionType: rule.ActCouponWake, ReasonType: rule.ReasonWakeDormant,
		Params: map[string]any{"member_id": int64(55), "coupon_id": int64(77)},
		Copy:   "今日推荐", MarginOK: true,
	})
	env, data := doM2(t, r, http.MethodPost, fmt.Sprintf("/v1/boss/aiengine/strategies/1/items/%d/execute", itemID), "1", "10", gin.H{"confirm": true})
	require.Equal(t, 0, env.Code)
	require.Equal(t, strategy.ItemExecuted, data["status"])
	require.EqualValues(t, 9001, data["ref_id"])
}

// TestM3S1_Execute_CouponWake_Blocked: reach blocked(freq_circuit) → ExecResult
// blocked + item lands ItemBlocked (NOT executed).
func TestM3S1_Execute_CouponWake_Blocked(t *testing.T) {
	sender := &m3s1Sender{ref: &exec.SendRef{TaskID: 7001, Status: "blocked", BlockedReason: "freq_circuit"}}
	r, svc := newM3S1Router(m3s1Lookuper{}, nil, nil, &m3s1Coupons{}, sender, nil)
	_, itemID := seedItem(t, svc, 1, 10, strategy.StrategyItem{
		Rank: 1, ActionType: rule.ActCouponWake, ReasonType: rule.ReasonWakeDormant,
		Params: map[string]any{"member_id": int64(55), "coupon_id": int64(77)},
		Copy:   "今日推荐", MarginOK: true,
	})
	env, data := doM2(t, r, http.MethodPost, fmt.Sprintf("/v1/boss/aiengine/strategies/1/items/%d/execute", itemID), "1", "10", gin.H{"confirm": true})
	require.Equal(t, 0, env.Code)
	require.Equal(t, strategy.ItemBlocked, data["status"])
	require.Equal(t, "freq_circuit", data["reason"])
}

// TestM3S1_Execute_PriceAdj_MarginFloor: new price below floor → blocked(margin_floor).
func TestM3S1_Execute_PriceAdj_MarginFloor(t *testing.T) {
	r, svc := newM3S1Router(m3s1Lookuper{}, nil, nil, nil, nil, &m3s1Pricer{})
	_, itemID := seedItem(t, svc, 1, 10, strategy.StrategyItem{
		Rank: 1, ActionType: rule.ActPriceAdj, ReasonType: rule.ReasonClearanceCoupon, ProductID: 100,
		Params: map[string]any{"cost_cents": int64(1200), "price_cents": int64(1100)}, // 1100 < 1200 → below floor
		Copy:   "调价", MarginOK: true,
	})
	env, data := doM2(t, r, http.MethodPost, fmt.Sprintf("/v1/boss/aiengine/strategies/1/items/%d/execute", itemID), "1", "10", gin.H{"confirm": true})
	require.Equal(t, 0, env.Code)
	require.Equal(t, strategy.ItemBlocked, data["status"])
	require.Equal(t, "margin_floor", data["reason"])
}

// TestM3S1_CrossTenant: tenant B cannot read tenant A's strategy → 40404.
func TestM3S1_CrossTenant(t *testing.T) {
	r, svc := newM3S1Router(m3s1Lookuper{}, nil, nil, nil, nil, nil)
	stratID, _ := seedItem(t, svc, 1, 10, strategy.StrategyItem{
		Rank: 1, ActionType: rule.ActCouponWake, ReasonType: rule.ReasonWakeDormant,
		Params: map[string]any{"member_id": int64(55), "coupon_id": int64(77)},
		Copy:   "x", MarginOK: true,
	})
	// tenant 2 reads tenant 1's strategy → not found
	env, _ := doM2(t, r, http.MethodGet, fmt.Sprintf("/v1/boss/aiengine/strategies/%d", stratID), "2", "20", nil)
	require.Equal(t, 40404, env.Code)
}
