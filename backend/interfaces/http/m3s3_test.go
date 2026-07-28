package httpserver

// M3-S3 HTTP-layer tests (CONTRACT §14.6 / §14.7 / §14.9):
//   ① Playbook cold-start injection — empty signal + a wired playbook store
//      → degrade="cold_start" and the strategy items are the configured
//      business's ColdPriority action_types (fruit → moments_post /
//      group_seckill / groupbuy), not the M3-S1 generic coupon_wake pair.
//   ② Attribution read API (FE-ADMIN-03 backend) — GET
//      /v1/boss/aiengine/strategies/:id/attribution aggregates the latest
//      strategy_attribution rows per item; missing metrics surface as null.
//   ③ Cross-tenant GET attribution → 40404 (absence indistinguishable from
//      foreign ownership, ARCH-01).
//   ④ groupbuy_success emit — groupbuy.Service.Create(StrategyID) + Join to
//      success → Compute → Repo.Record → GET /attribution surfaces
//      groupbuy_success non-null (>0) by strategy_id linkage (§14.9).
//   ⑤ revenue_cny best-effort — member_coupons(strategy_id) + paid order →
//      GET /attribution surfaces revenue_cny non-null (>0) (handler computes
//      on the fly via marketing.MembersByStrategy + billing.OrderTotalByMembers).
//   ⑥ repurchase emit — member_coupons(strategy_id) + repurchase paid order in
//      the [executedAt, now] window → Compute (RepurchaseCounter port) →
//      Repo.Record → GET /attribution surfaces repurchase non-null (§14.9).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/aiengine"
	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/cache"
	"github.com/zhangguijunshi/backend/internal/aiengine/converge"
	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/playbook"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// newM3S3Router wires a pipeline WITH the playbook store + an attribution
// MemoryRepo so the cold-start injection + read API are exercisable over HTTP.
// It also wires real marketing/billing/groupbuy services (memory repos, nil
// optional lookuper/guard/pricer-adapter ports — dev convenience) so the M3-S3
// 收尾 best-effort revenue_cny / repurchase / groupbuy_success chain is
// exercisable end-to-end. Returns every service so a test can seed domain data
// (groupbuys, member_coupons, paid orders) before GET /attribution.
func newM3S3Router(lk signal.Lookuper) (*gin.Engine, *strategy.Service, attribution.Repo, *marketing.Service, *billing.Service, *groupbuy.Service) {
	strategySvc := strategy.New(strategy.NewMemoryStrategyRepo())
	guard := margin.Guard{MarginFloorPct: 5}
	collector := signal.NewCollector("fruit", "", lk)
	ruleEngine := rule.NewEngine(rule.DefaultSeeds(), guard)
	gw, _ := gateway.NewFromConfig(config.AIConfig{}) // empty → no_provider degrade
	pipeline := aiengine.NewPipeline(
		collector, ruleEngine, agent.New(gw), converge.NewTop3(),
		output.NewEncoder(strategySvc), cache.NewMemoryCache(),
		aiengine.WithPlaybookStore(playbook.NewStore()),
	)
	attrRepo := attribution.NewMemoryRepo()
	// Real domain services with memory repos + nil optional anti-corruption
	// ports (member lookuper / margin guard / stock consumer) so the test can
	// issue coupons + create orders + open groupbuys without a full
	// catalog/membership stack. m3s3Pricer is a thin active-product stub so
	// billing.CreateOrder prices lines deterministically.
	mktSvc := marketing.New(marketing.NewMemoryCouponRepo())
	billSvc := billing.New(billing.NewMemoryOrderRepo(), m3s3Pricer{priceCents: 500})
	gbSvc := groupbuy.New(groupbuy.NewMemoryGroupbuyRepo())
	return New(Deps{
		Cfg:         &config.Config{App: config.AppConfig{Env: "test"}},
		Strategy:    strategySvc,
		Pipeline:    pipeline,
		Attribution: attrRepo,
		Marketing:   mktSvc,
		Billing:     billSvc,
		DevMock:     true,
	}), strategySvc, attrRepo, mktSvc, billSvc, gbSvc
}

// m3s3Pricer is a billing.ProductPricer stub returning one active product at a
// fixed price for any (tenantID, productID). It lets billing.CreateOrder price
// lines in the smoke tests without a real catalog. Tenant is echoed back so
// the billing cross-tenant guard passes.
type m3s3Pricer struct{ priceCents int64 }

func (p m3s3Pricer) GetByID(_ context.Context, tenantID, id int64) (*billing.PricedProduct, error) {
	return &billing.PricedProduct{
		ID:         id,
		TenantID:   tenantID,
		Name:       "苹果",
		Unit:       "斤",
		PriceCents: p.priceCents,
		CostCents:  p.priceCents / 2,
		Status:     "active",
	}, nil
}

// m3s3TenantCtx injects tenantID as a system-sourced identity so the
// attribution Computer's anti-corruption ports (which re-derive tenant via
// multitenancy.EnforceTenant) see the same tenant the test seeds. Mirrors the
// cmd/api withTenantCtx adapter.
func m3s3TenantCtx(ctx context.Context, tenantID int64) context.Context {
	return multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		Source:   multitenancy.SourceSystem,
	})
}

// m3s3GroupbuyStats adapts groupbuy.Service.StatsByStrategy onto the
// attribution.GroupbuyStats port (the Computer queries it for groupbuy_success).
type m3s3GroupbuyStats struct{ svc *groupbuy.Service }

func (a m3s3GroupbuyStats) StartedSucceeded(ctx context.Context, tenantID, strategyID int64) (int, int, error) {
	if a.svc == nil || strategyID <= 0 {
		return 0, 0, nil
	}
	return a.svc.StatsByStrategy(m3s3TenantCtx(ctx, tenantID), strategyID)
}

// m3s3Repurchase adapts marketing.MembersByStrategy + billing.RepurchaseMembers
// onto the attribution.RepurchaseCounter port (the Computer queries it for the
// repurchase metric). Mirrors the cmd/api repurchaseCounterAdapter; degrades to
// (0, nil) on any gap (no panic, no cross-tenant leak).
type m3s3Repurchase struct {
	mkt  *marketing.Service
	bill *billing.Service
}

func (a m3s3Repurchase) Rate(ctx context.Context, tenantID, strategyID int64, executedAt, measuredAt time.Time) (float64, error) {
	if a.mkt == nil || a.bill == nil || strategyID <= 0 {
		return 0, nil
	}
	mctx := m3s3TenantCtx(ctx, tenantID)
	memberIDs, err := a.mkt.MembersByStrategy(mctx, tenantID, strategyID)
	if err != nil || len(memberIDs) == 0 {
		return 0, nil
	}
	n, err := a.bill.RepurchaseMembers(mctx, memberIDs, executedAt, measuredAt)
	if err != nil {
		return 0, nil
	}
	v := float64(n) / float64(len(memberIDs))
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v, nil
}

// m3s3SeedStrategy saves a one-item strategy for the ctx tenant/store with the
// given action_type / reason_type, then marks the item executed (stamping
// ExecutedAt=now so the revenue_cny / repurchase windows are valid). Returns the
// strategy + the executed item.
func m3s3SeedStrategy(t *testing.T, strategySvc *strategy.Service, tenantID, storeID int64, actionType, reasonType string) (*strategy.Strategy, *strategy.StrategyItem) {
	t.Helper()
	ctx := m3s3TenantCtx(context.Background(), tenantID)
	now := time.Now()
	today := now.Format("2006-01-02")
	st, err := strategySvc.SaveAdvice(ctx, &strategy.Strategy{
		TenantID:    tenantID,
		StoreID:     storeID,
		ForDate:     today,
		GeneratedBy: strategy.GenRuleOnly,
		Items: []strategy.StrategyItem{{
			Rank:       1,
			ActionType: actionType,
			ReasonType: reasonType,
		}},
	})
	require.NoError(t, err)
	require.Len(t, st.Items, 1)
	item, err := strategySvc.UpdateItemStatus(ctx, st.Items[0].ID, strategy.ItemExecuted)
	require.NoError(t, err)
	return st, item
}

// fruitColdPriority is the fruit playbook's ColdPriority action set (seed.go).
var fruitColdPriority = map[string]bool{
	rule.ActMomentsPost:  true,
	rule.ActGroupSeckill: true,
	rule.ActGroupbuy:     true,
}

// TestM3S3_PlaybookColdStart: empty signal + wired playbook store → cold_start
// with the fruit ColdPriority action_types (not the generic coupon_wake pair).
func TestM3S3_PlaybookColdStart(t *testing.T) {
	r, _, _, _, _, _ := newM3S3Router(m3s1Lookuper{}) // empty → no rule matches → cold-start

	env, data := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", "1", "10", gin.H{})
	require.Equal(t, 0, env.Code)
	require.Equal(t, "cold_start", data["degrade"])

	strat, _ := data["strategy"].(map[string]any)
	require.NotNil(t, strat)
	items, _ := strat["items"].([]any)
	require.GreaterOrEqual(t, len(items), 1, "cold-start must inject ≥1 item")

	// Every injected item's action_type must be in the fruit ColdPriority set.
	for _, it := range items {
		m, _ := it.(map[string]any)
		act, _ := m["action_type"].(string)
		require.True(t, fruitColdPriority[act], "cold-start item action %q not in fruit ColdPriority", act)
	}
}

// TestM3S3_AttributionReadAPI: generate a strategy, seed attribution rows,
// then GET /attribution aggregates the latest value per metric per item and
// surfaces missing metrics as absent (null in the JSON).
func TestM3S3_AttributionReadAPI(t *testing.T) {
	r, _, attrRepo, _, _, _ := newM3S3Router(m3s1Lookuper{})

	// Generate today's advice (cold-start) for tenant 1 / store 10.
	env, data := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", "1", "10", gin.H{})
	require.Equal(t, 0, env.Code)
	strat, _ := data["strategy"].(map[string]any)
	require.NotNil(t, strat)
	stratID, _ := strat["id"].(float64)
	require.Greater(t, stratID, 0.0)
	items, _ := strat["items"].([]any)
	require.GreaterOrEqual(t, len(items), 1)

	// Seed a coupon_redeem measurement for the first item (0.5 = 50% redeem rate).
	firstItem, _ := items[0].(map[string]any)
	itemID := int64(firstItem["id"].(float64))
	_, err := attrRepo.Record(context.Background(), 1, itemID, attribution.MetricCouponRedeem, 0.5, time.Now())
	require.NoError(t, err)

	// GET the attribution dashboard payload.
	env, attr := doM2(t, r, http.MethodGet,
		"/v1/boss/aiengine/strategies/"+itoa(int64(stratID))+"/attribution", "1", "10", nil)
	require.Equal(t, 0, env.Code)
	require.Equal(t, float64(stratID), attr["strategy_id"])

	itemsOut, _ := attr["items"].([]any)
	require.Len(t, itemsOut, len(items))
	first, _ := itemsOut[0].(map[string]any)
	require.Equal(t, firstItem["action_type"], first["action_type"])
	// coupon_redeem present (0.5); visit_uplift / groupbuy_success absent (no data).
	cr, ok := first["coupon_redeem"].(float64)
	require.True(t, ok, "coupon_redeem must be present")
	require.InDelta(t, 0.5, cr, 1e-9)
	_, hasVisit := first["visit_uplift"]
	require.False(t, hasVisit, "visit_uplift must be absent (no measurement)")
}

// TestM3S3_AttributionCrossTenant: a strategy owned by tenant 1 must be
// invisible to tenant 2 → GET attribution returns 40404 (errcode
// ErrStrategyNotFound, ARCH-01: absence == foreign ownership).
func TestM3S3_AttributionCrossTenant(t *testing.T) {
	r, _, attrRepo, _, _, _ := newM3S3Router(m3s1Lookuper{})

	env, data := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", "1", "10", gin.H{})
	require.Equal(t, 0, env.Code)
	strat, _ := data["strategy"].(map[string]any)
	stratID := int64(strat["id"].(float64))
	items, _ := strat["items"].([]any)
	itemID := int64(items[0].(map[string]any)["id"].(float64))
	_, _ = attrRepo.Record(context.Background(), 1, itemID, attribution.MetricCouponRedeem, 0.5, time.Now())

	// Tenant 2 asks for tenant 1's strategy → 40404 (not 200 with data).
	env2, _ := doM2(t, r, http.MethodGet,
		"/v1/boss/aiengine/strategies/"+itoa(stratID)+"/attribution", "2", "20", nil)
	require.Equal(t, 40404, env2.Code)
}

// TestM3S3_GroupbuySuccessEmit (④): create a groupbuy whose StrategyID is the
// strategy's id, join it to success, run the attribution Computer (GroupbuyStats
// port proxying groupbuy.StatsByStrategy), Record the measurement, then GET
// /attribution surfaces groupbuy_success non-null and >0 (strategy_id linkage,
// §14.9). Proves the M3-S3 收尾 groupbuy→strategy linkage is wired end-to-end.
func TestM3S3_GroupbuySuccessEmit(t *testing.T) {
	r, strategySvc, attrRepo, _, _, gbSvc := newM3S3Router(m3s1Lookuper{})

	strat, item := m3s3SeedStrategy(t, strategySvc, 1, 10, rule.ActGroupbuy, rule.ReasonBundleSlow)
	ctx := m3s3TenantCtx(context.Background(), 1)

	// Open a groupbuy attributed to the strategy (RequiredCount=1 → one join
	// flips it to success).
	g, err := gbSvc.Create(ctx, groupbuy.CreateInput{
		StoreID:       10,
		ProductID:     1,
		StrategyID:    strat.ID,
		Title:         "苹果拼团",
		RequiredCount: 1,
		ValidHours:    24,
	})
	require.NoError(t, err)
	// Join → squad flips to success (Joined reaches RequiredCount).
	_, g2, err := gbSvc.Join(ctx, g.ID, 8001)
	require.NoError(t, err)
	require.Equal(t, groupbuy.StatusSuccess, g2.Status)

	// Run the Computer with the groupbuy port → groupbuy_success measurement.
	computer := attribution.NewComputer(attribution.Sources{
		Groupbuy: m3s3GroupbuyStats{svc: gbSvc},
	}, nil)
	executedAt := time.Now()
	measuredAt := executedAt
	ms, err := computer.Compute(ctx, 1, item, 10, executedAt, measuredAt)
	require.NoError(t, err)
	require.Len(t, ms, 1, "groupbuy item must emit exactly the groupbuy_success metric")
	var succ float64
	var emitted bool
	for _, m := range ms {
		if m.Metric == attribution.MetricGroupbuySuccess {
			emitted = true
			succ = m.Value
		}
	}
	require.True(t, emitted, "groupbuy_success must be emitted")
	require.InDelta(t, 1.0, succ, 1e-9, "1 started / 1 succeeded → 1.0")
	for _, m := range ms {
		_, rerr := attrRepo.Record(ctx, 1, item.ID, m.Metric, m.Value, m.MeasuredAt)
		require.NoError(t, rerr)
	}

	// GET /attribution → groupbuy_success non-null (>0).
	env, attr := doM2(t, r, http.MethodGet,
		"/v1/boss/aiengine/strategies/"+itoa(strat.ID)+"/attribution", "1", "10", nil)
	require.Equal(t, 0, env.Code)
	items, _ := attr["items"].([]any)
	require.Len(t, items, len(strat.Items))
	first, _ := items[0].(map[string]any)
	gs, ok := first["groupbuy_success"].(float64)
	require.True(t, ok, "groupbuy_success must be present")
	require.InDelta(t, 1.0, gs, 1e-9)
}

// TestM3S3_RevenueCNYBestEffort (⑤): issue a member-coupon attributed to the
// strategy (strategy_id) and create a paid order for that member → GET
// /attribution surfaces revenue_cny non-null (>0). The handler computes
// revenue_cny on the fly (marketing.MembersByStrategy + billing.OrderTotalByMembers
// over [ExecutedAt, now]); no Compute/Record step is needed.
func TestM3S3_RevenueCNYBestEffort(t *testing.T) {
	r, strategySvc, _, mktSvc, billSvc, _ := newM3S3Router(m3s1Lookuper{})

	strat, item := m3s3SeedStrategy(t, strategySvc, 1, 10, rule.ActCouponWake, rule.ReasonWakeDormant)
	ctx := m3s3TenantCtx(context.Background(), 1)
	now := time.Now()

	// Active cash coupon, then issue it to member 8001 under strategy_id.
	c, err := mktSvc.CreateCoupon(ctx, marketing.CreateCouponInput{
		StoreID: 10,
		Name:    "revenue-smoke-coupon",
		Type:    marketing.TypeCash,
		Params:  marketing.CouponParams{OffCents: 50},
		ValidTo: now.Add(30 * 24 * time.Hour),
	})
	require.NoError(t, err)
	_, err = mktSvc.UpdateStatus(ctx, c.ID, marketing.StatusActive)
	require.NoError(t, err)
	_, err = mktSvc.Issue(ctx, marketing.IssueInput{
		MemberID:   8001,
		CouponID:   c.ID,
		Source:     marketing.SourceAIStrategy,
		StrategyID: strat.ID,
	})
	require.NoError(t, err)

	// Paid order for member 8001: 2 × priceCents 500 = 1000 cents = ¥10.00.
	_, err = billSvc.CreateOrder(ctx, billing.CreateOrderInput{
		StoreID:  10,
		MemberID: 8001,
		Items:    []billing.CreateOrderItem{{ProductID: 1, Qty: 2}},
	})
	require.NoError(t, err)

	// GET /attribution → revenue_cny = 1000/100 = 10.0.
	env, attr := doM2(t, r, http.MethodGet,
		"/v1/boss/aiengine/strategies/"+itoa(strat.ID)+"/attribution", "1", "10", nil)
	require.Equal(t, 0, env.Code)
	items, _ := attr["items"].([]any)
	require.Len(t, items, len(strat.Items))
	first, _ := items[0].(map[string]any)
	rev, ok := first["revenue_cny"].(float64)
	require.True(t, ok, "revenue_cny must be present (best-effort)")
	require.InDelta(t, 10.0, rev, 1e-9)
	_ = item // executed item, window [ExecutedAt, now] holds the order
}

// TestM3S3_RepurchaseEmit (⑥): issue a member-coupon attributed to the strategy
// + a repurchase paid order in the [executedAt, now] window → run the Computer
// (RepurchaseCounter port) → Record → GET /attribution surfaces repurchase
// non-null (§14.9). Proves the M3-S3 收尾 repurchase best-effort chain.
func TestM3S3_RepurchaseEmit(t *testing.T) {
	r, strategySvc, attrRepo, mktSvc, billSvc, _ := newM3S3Router(m3s1Lookuper{})

	strat, item := m3s3SeedStrategy(t, strategySvc, 1, 10, rule.ActCouponWake, rule.ReasonWakeDormant)
	ctx := m3s3TenantCtx(context.Background(), 1)
	now := time.Now()

	// Active coupon + issue to member 8001 under strategy_id.
	c, err := mktSvc.CreateCoupon(ctx, marketing.CreateCouponInput{
		StoreID: 10,
		Name:    "repurchase-smoke-coupon",
		Type:    marketing.TypeCash,
		Params:  marketing.CouponParams{OffCents: 50},
		ValidTo: now.Add(30 * 24 * time.Hour),
	})
	require.NoError(t, err)
	_, err = mktSvc.UpdateStatus(ctx, c.ID, marketing.StatusActive)
	require.NoError(t, err)
	_, err = mktSvc.Issue(ctx, marketing.IssueInput{
		MemberID:   8001,
		CouponID:   c.ID,
		Source:     marketing.SourceAIStrategy,
		StrategyID: strat.ID,
	})
	require.NoError(t, err)

	// Repurchase: a paid order for the same member in the window.
	_, err = billSvc.CreateOrder(ctx, billing.CreateOrderInput{
		StoreID:  10,
		MemberID: 8001,
		Items:    []billing.CreateOrderItem{{ProductID: 1, Qty: 1}},
	})
	require.NoError(t, err)

	// Run the Computer with the repurchase port → repurchase measurement. The
	// window [executedAt, measuredAt] must contain the order's PaidAt, so
	// measuredAt is sampled AFTER the order is created (the order's PaidAt is
	// stamped at CreateOrder time, later than the seed's ExecutedAt).
	computer := attribution.NewComputer(attribution.Sources{
		Repurchase: m3s3Repurchase{mkt: mktSvc, bill: billSvc},
	}, nil)
	executedAt := *item.ExecutedAt
	measuredAt := time.Now()
	ms, err := computer.Compute(ctx, 1, item, 10, executedAt, measuredAt)
	require.NoError(t, err)
	var rate float64
	var emitted bool
	for _, m := range ms {
		if m.Metric == attribution.MetricRepurchase {
			emitted = true
			rate = m.Value
		}
	}
	require.True(t, emitted, "repurchase must be emitted")
	require.InDelta(t, 1.0, rate, 1e-9, "1/1 member repurchased → 1.0")
	for _, m := range ms {
		_, rerr := attrRepo.Record(ctx, 1, item.ID, m.Metric, m.Value, m.MeasuredAt)
		require.NoError(t, rerr)
	}

	// GET /attribution → repurchase non-null.
	env, attr := doM2(t, r, http.MethodGet,
		"/v1/boss/aiengine/strategies/"+itoa(strat.ID)+"/attribution", "1", "10", nil)
	require.Equal(t, 0, env.Code)
	items, _ := attr["items"].([]any)
	require.Len(t, items, len(strat.Items))
	first, _ := items[0].(map[string]any)
	rp, ok := first["repurchase"].(float64)
	require.True(t, ok, "repurchase must be present")
	require.InDelta(t, 1.0, rp, 1e-9)
}
