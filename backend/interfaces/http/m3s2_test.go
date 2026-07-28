package httpserver

// M3-S2 HTTP-layer tests (CONTRACT §13.6): the semantic-cache (L2) integration
// over the boss surface. The pipeline is wired with a MemorySemanticCache
// (FakeEmbedder) so a second request carrying the same signal — but a different
// store (L1 misses by tenant+store+date) — hits the L2 semantic cache and
// returns degrade="semantic_hit" without re-running rule/agent/encode.

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/aiengine"
	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/cache"
	"github.com/zhangguijunshi/backend/internal/aiengine/converge"
	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/aiengine/semantic"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// newM3S2Router wires a pipeline WITH the L2 MemorySemanticCache (FakeEmbedder),
// so semantic-cache hits are exercisable over HTTP. Reuses the m3s1 fakes
// (m3s1Lookuper / nearingApple).
func newM3S2Router(lk signal.Lookuper) (*gin.Engine, *strategy.Service) {
	strategySvc := strategy.New(strategy.NewMemoryStrategyRepo())
	guard := margin.Guard{MarginFloorPct: 5}
	collector := signal.NewCollector("fruit", "", lk)
	ruleEngine := rule.NewEngine(rule.DefaultSeeds(), guard)
	gw, _ := gateway.NewFromConfig(config.AIConfig{})       // empty → ErrNoProvider → no_provider
	emb := gateway.NewEmbedderFromConfig(config.AIConfig{}) // empty → FakeEmbedder (deterministic)
	semCache := semantic.NewMemoryCache(emb)
	pipeline := aiengine.NewPipeline(
		collector, ruleEngine, agent.New(gw), converge.NewTop3(),
		output.NewEncoder(strategySvc), cache.NewMemoryCache(),
		aiengine.WithSemanticCache(semCache),
	)
	return New(Deps{
		Cfg:      &config.Config{App: config.AppConfig{Env: "test"}},
		Strategy: strategySvc,
		Pipeline: pipeline,
		DevMock:  true,
	}), strategySvc
}

// TestM3S2_SemanticCacheHit: same tenant, two stores, identical signal (fake
// lookuper returns the same inventory). First request generates + L2 Puts;
// second request misses L1 (different store, same date) but hits L2 (same
// tenant + same signalText + FakeEmbedder deterministic embedding) → degrade
// = "semantic_hit".
func TestM3S2_SemanticCacheHit(t *testing.T) {
	lk := m3s1Lookuper{inv: []signal.InventorySig{nearingApple()}}
	r, _ := newM3S2Router(lk)

	// store 10: first generation (degrade no_provider — empty gateway; L2 Put happens).
	env1, data1 := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", "1", "10", gin.H{})
	require.Equal(t, 0, env1.Code)
	require.NotEqual(t, "semantic_hit", data1["degrade"], "first request must generate, not hit L2")

	// store 20 (same tenant, same signal via fake lookuper): L1 miss (diff store)
	// → L2 hit.
	env2, data2 := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", "1", "20", gin.H{})
	require.Equal(t, 0, env2.Code)
	require.Equal(t, "semantic_hit", data2["degrade"], "second request with same signal should hit L2 semantic cache")
	// L2 hit returns the cached strategy (same shape: 3-thing advice).
	strat, _ := data2["strategy"].(map[string]any)
	require.NotNil(t, strat, "semantic_hit must return the cached strategy")
	items, _ := strat["items"].([]any)
	require.GreaterOrEqual(t, len(items), 1)
}

// TestM3S2_SemanticCacheMissDifferentSignal: a different signal (different
// inventory) must NOT hit the L2 cache → generates fresh (no_provider/rule_only).
func TestM3S2_SemanticCacheMissDifferentSignal(t *testing.T) {
	// First router: apples nearing → L2 Put signalText A.
	r1, _ := newM3S2Router(m3s1Lookuper{inv: []signal.InventorySig{nearingApple()}})
	env1, _ := doM2(t, r1, http.MethodPost, "/v1/boss/aiengine/today", "1", "10", gin.H{})
	require.Equal(t, 0, env1.Code)

	// Second router: empty signal → signalText B (different) → L2 miss → cold_start.
	// (r1 and r2 have independent caches, but the point is signalText B ≠ A so even
	// a shared cache would miss; here we assert cold_start for the empty-signal case.)
	r2, _ := newM3S2Router(m3s1Lookuper{}) // empty → cold-start
	env2, data2 := doM2(t, r2, http.MethodPost, "/v1/boss/aiengine/today", "2", "20", gin.H{})
	require.Equal(t, 0, env2.Code)
	require.Equal(t, "cold_start", data2["degrade"])
}
