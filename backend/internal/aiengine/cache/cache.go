// Package cache is the L1 advice cache for the AI engine pipeline
// (CONTRACT §12.3.4). It stores already-persisted *strategy.Strategy snapshots
// keyed by (tenantID, storeID, date) so that a same-day re-run of the pipeline
// short-circuits at the very first stage (cache hit → Advice.Degrade=cache_hit,
// skipping signal/rule/LLM entirely).
//
// This milestone ships an in-memory exact-hash implementation (MemoryCache).
// The PGVector semantic-cache implementation (AI-06) will satisfy the same
// Cache interface in M3-S2; callers stay unaware of the swap.
package cache

import (
	"context"

	"github.com/zhangguijunshi/backend/internal/strategy"
)

// Cache is the L1 advice cache. Key = (tenantID, storeID, date). Put must not
// block the pipeline on failure — callers swallow its error (a cache miss just
// means a cold run).
type Cache interface {
	// TryHit returns the cached advice for (tenant, store, date) plus true on a
	// hit; (nil, false) on a miss. The returned *Strategy is a defensive deep
	// copy so callers cannot mutate the stored snapshot.
	TryHit(ctx context.Context, tenantID, storeID int64, date string) (*strategy.Strategy, bool)

	// Put stores (or overwrites) the advice for (tenant, store, date). A nil
	// strategy is a no-op. The stored snapshot is a defensive deep copy.
	Put(ctx context.Context, tenantID, storeID int64, date string, s *strategy.Strategy)
}
