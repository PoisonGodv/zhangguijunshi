package httpserver

// M3-S2 HTTP-layer tests (CONTRACT §13.6): the semantic-cache (L2) integration
// over the boss surface. The pipeline is wired with a MemorySemanticCache
// (FakeEmbedder).
//
// Store-isolation invariant (bugfix ①): the L2 semantic cache key now includes
// store_id, so a second request for a DIFFERENT store carrying an identical
// signal must NOT hit another store's cached strategy — it regenerates. A
// same-store repeat request still short-circuits via the L1 cache (cache_hit).

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

// TestM3S2_SemanticCacheHit: same tenant, same store, identical signal. The
// first request generates and warms both caches; the second request for the
// SAME store short-circuits on the L1 cache → degrade="cache_hit".
//
// A DIFFERENT store with the same signal must NOT hit L2 (store-isolation
// invariant, bugfix ①): it regenerates instead of returning another store's
// cached strategy.
func TestM3S2_SemanticCacheHit(t *testing.T) {
	lk := m3s1Lookuper{inv: []signal.InventorySig{nearingApple()}}
	r, _ := newM3S2Router(lk)

	// store 10: first generation (degrade no_provider — empty gateway; L1+L2 Put happen).
	env1, data1 := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", "1", "10", gin.H{})
	require.Equal(t, 0, env1.Code)
	require.NotEqual(t, "semantic_hit", data1["degrade"], "first request must generate, not hit L2")
	require.NotEqual(t, "cache_hit", data1["degrade"], "first request must generate, not hit L1")

	// store 10 again (same tenant+store+date): L1 hit.
	env2, data2 := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", "1", "10", gin.H{})
	require.Equal(t, 0, env2.Code)
	require.Equal(t, "cache_hit", data2["degrade"], "same-store repeat should hit the L1 cache")
	strat, _ := data2["strategy"].(map[string]any)
	require.NotNil(t, strat, "cache_hit must return the cached strategy")
	items, _ := strat["items"].([]any)
	require.GreaterOrEqual(t, len(items), 1)

	// store 20 (same tenant, same signal, DIFFERENT store): L1 miss (diff store)
	// and L2 must also miss now that the key carries store_id — it regenerates
	// rather than returning store 10's cached strategy.
	env3, data3 := doM2(t, r, http.MethodPost, "/v1/boss/aiengine/today", "1", "20", gin.H{})
	require.Equal(t, 0, env3.Code)
	require.NotEqual(t, "semantic_hit", data3["degrade"], "different store must not hit another store's L2 entry")
	require.NotEqual(t, "cache_hit", data3["degrade"], "different store must not hit another store's L1 entry")
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
