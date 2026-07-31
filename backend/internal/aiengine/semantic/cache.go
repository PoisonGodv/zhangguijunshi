// Package semantic implements the L2 semantic advice cache for the AI engine
// pipeline (CONTRACT §13.3.4, tech plan ch07 §7.6). It sits behind the L1
// exact-hash cache (internal/aiengine/cache): when L1 misses, the pipeline
// probes L2 by the embedding of a "signal text" — a compact string built from
// the tenant's business + signal dimensions (e.g. "fruit|weather=rain|
// days_to_expiry=2|sleep_30=18|holiday=none"). Two different days that resolve
// to semantically near-identical signal texts ("水果店+雨天+临期葡萄" vs
// "水果店+雨天+临期苹果") share one advice draft, slashing LLM cost without
// surfacing stale advice to the boss.
//
// Two implementations ship in this package (CONTRACT §13.5.3):
//
//   - MemorySemanticCache: dev/test. Embeds signal text with the injected
//     gateway.Embedder, then linear-scans an in-memory slice for cosine
//     similarity >0.85 (same tenant + same business + not expired). No PG
//     dependency; FakeEmbedder makes identical texts hit deterministically.
//
//   - PGVectorSemanticCache: production. Stores embeddings in the ai_cache
//     table (migration 000008, vector(1024) + HNSW index) and queries with the
//     pgvector cosine-distance operator `<=>` (<0.15 == similarity >0.85).
//     The dependency is a narrow Pool interface (QueryRow/Exec) so tests run
//     with a fake; the main thread injects *pgxpool.Pool (after registering
//     the pgvector type) in production and MemorySemanticCache in dev.
//
// Hit/miss counts flow through Metrics (prometheus ai_cache_hit_total /
// ai_cache_miss_total, per-tenant label) which the pipeline threads through
// both implementations.
package semantic

import (
	"context"
	"time"

	"github.com/zhangguijunshi/backend/internal/strategy"
)

// SemanticCache is the L2 semantic advice cache (CONTRACT §13.3.4). The L1
// exact-hash cache (internal/aiengine/cache.Cache) is keyed by
// (tenant, store, date); this L2 is keyed by the EMBEDDING of a signal text,
// so it can return advice from a different day whose signals were semantically
// near-identical. All methods take tenantID AND storeID explicitly — neither is
// ever trusted from the strategy payload or the signal text (ARCH-01 /
// §13.5.1). storeID is part of the retrieval key (migration 000022): a
// multi-store tenant's advice is store-specific (it references that store's
// products / members), so a near-identical signal on a DIFFERENT store must NOT
// hit another store's cached draft — doing so would surface one store's advice
// to another (high-severity cross-store leak).
//
// TryHit returns a defensive deep copy of the cached advice on a hit
// (strategy.CloneStrategy for Memory; a freshly unmarshalled value for
// PGVector) so callers cannot mutate the stored snapshot. On a hit the cache
// also stamps Metrics.IncHit; on a miss, Metrics.IncMiss.
type SemanticCache interface {
	// TryHit probes the cache by the embedding of signalText, filtered to
	// tenantID + storeID + business and entries whose expires_at > now. Returns
	// (advice, true, nil) on a hit; (nil, false, nil) on a miss; a non-nil
	// error only when embedding or storage genuinely fails (a storage miss
	// is NOT an error — it just means a cold run).
	TryHit(ctx context.Context, tenantID, storeID int64, business, signalText string) (*strategy.Strategy, bool, error)

	// Put stores s (defensive copy) under the embedding of signalText, keyed by
	// (tenantID, storeID, business), with expires_at = now + ttl. A nil s is a
	// no-op. Put must not block the pipeline on failure — callers swallow its
	// error (a failed write just means the next run is cold).
	Put(ctx context.Context, tenantID, storeID int64, business, signalText string, s *strategy.Strategy, ttl time.Duration) error

	// Invalidate expires every entry for tenantID (Memory: drops rows;
	// PGVector: UPDATE expires_at=now() WHERE tenant_id=$1). Called after the
	// merchant executes an action so the engine does not re-recommend the
	// same draft (§7.6.4).
	Invalidate(ctx context.Context, tenantID int64) error
}

// Metrics counts L2 semantic-cache hit/miss events for the Grafana hit-rate
// panel (§7.6.5). Implementations must be safe for concurrent use.
type Metrics interface {
	// IncHit records one cache hit for tenantID.
	IncHit(tenantID int64)
	// IncMiss records one cache miss for tenantID.
	IncMiss(tenantID int64)
}

// Counters is an optional Metrics extension that exposes raw hit/miss totals
// for test assertions and operational introspection. NewMetrics() returns a
// Metrics value that also satisfies Counters.
type Counters interface {
	Hits() int64
	Misses() int64
}

// nowFunc returns the current time. It is a package-level indirection so tests
// can advance a clock without touching time.Now globally. Each cache holds its
// own now closure (settable via WithClock).
type nowFunc func() time.Time

// defaultNow is the production clock.
func defaultNow() time.Time { return time.Now() }
