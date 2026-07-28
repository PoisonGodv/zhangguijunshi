package cache

// MemoryCache is the in-memory, concurrency-safe Cache (CONTRACT §12.3.4). It
// is the L1 exact-hash layer: key = (tenantID, storeID, date) → *strategy.
// Strategy. Reads return defensive deep copies (via strategy.CloneStrategy) so
// callers — the pipeline, the handler — cannot mutate a stored snapshot, and a
// later mutation of the source *Strategy after Put does not leak into the cache.

import (
	"context"
	"sync"

	"github.com/zhangguijunshi/backend/internal/strategy"
)

// cacheKey is the (tenant, store, date) upsert key.
type cacheKey struct {
	tenantID int64
	storeID  int64
	date     string
}

// MemoryCache holds strategy snapshots behind a RWMutex.
type MemoryCache struct {
	mu   sync.RWMutex
	data map[cacheKey]*strategy.Strategy
}

// NewMemoryCache constructs an empty in-memory Cache.
func NewMemoryCache() *MemoryCache {
	return &MemoryCache{data: make(map[cacheKey]*strategy.Strategy)}
}

// TryHit returns a defensive copy of the cached advice on a hit; (nil, false)
// on a miss.
func (c *MemoryCache) TryHit(_ context.Context, tenantID, storeID int64, date string) (*strategy.Strategy, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.data[cacheKey{tenantID: tenantID, storeID: storeID, date: date}]
	if !ok || s == nil {
		return nil, false
	}
	return strategy.CloneStrategy(s), true
}

// Put stores a defensive copy of s under (tenant, store, date). A nil s is a
// no-op.
func (c *MemoryCache) Put(_ context.Context, tenantID, storeID int64, date string, s *strategy.Strategy) {
	if s == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[cacheKey{tenantID: tenantID, storeID: storeID, date: date}] = strategy.CloneStrategy(s)
}
