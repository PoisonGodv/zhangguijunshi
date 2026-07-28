package httpserver

// M3-S3 HTTP-layer tests (CONTRACT §14.6 / §14.7):
//   ① Playbook cold-start injection — empty signal + a wired playbook store
//      → degrade="cold_start" and the strategy items are the configured
//      business's ColdPriority action_types (fruit → moments_post /
//      group_seckill / groupbuy), not the M3-S1 generic coupon_wake pair.
//   ② Attribution read API (FE-ADMIN-03 backend) — GET
//      /v1/boss/aiengine/strategies/:id/attribution aggregates the latest
//      strategy_attribution rows per item; missing metrics surface as null.
//   ③ Cross-tenant GET attribution → 40404 (absence indistinguishable from
//      foreign ownership, ARCH-01).

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
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// newM3S3Router wires a pipeline WITH the playbook store + an attribution
// MemoryRepo so the cold-start injection + read API are exercisable over HTTP.
// Returns the attribution repo so the test can seed measurements directly.
func newM3S3Router(lk signal.Lookuper) (*gin.Engine, *strategy.Service, attribution.Repo) {
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
	return New(Deps{
		Cfg:         &config.Config{App: config.AppConfig{Env: "test"}},
		Strategy:    strategySvc,
		Pipeline:    pipeline,
		Attribution: attrRepo,
		DevMock:     true,
	}), strategySvc, attrRepo
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
	r, _, _ := newM3S3Router(m3s1Lookuper{}) // empty → no rule matches → cold-start

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
	r, _, attrRepo := newM3S3Router(m3s1Lookuper{})

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
	r, _, attrRepo := newM3S3Router(m3s1Lookuper{})

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
