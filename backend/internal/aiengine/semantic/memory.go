package semantic

// memory.go implements MemorySemanticCache — the dev/test L2 semantic cache.
// It embeds signalText via the injected gateway.Embedder (FakeEmbedder in dev,
// deterministic), stores the vector + advice in an in-memory slice, and on
// TryHit linear-scans for the best cosine similarity exceeding the threshold
// (0.85). No PG dependency; concurrency-safe via RWMutex; returns defensive
// deep copies via strategy.CloneStrategy.

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// memEntry is one cached advice row in MemorySemanticCache.
type memEntry struct {
	tenantID  int64
	business  string
	embedding []float32
	advice    *strategy.Strategy
	expiresAt time.Time
}

// MemorySemanticCache is the in-memory L2 semantic cache (CONTRACT §13.3.4 /
// §13.5.3). It is the dev/test default: main thread injects it when no PG is
// available. Reads return defensive deep copies (strategy.CloneStrategy) so
// callers — the pipeline, the handler — cannot mutate a stored snapshot.
type MemorySemanticCache struct {
	embedder gateway.Embedder
	mu       sync.RWMutex
	entries  []memEntry
	metrics  Metrics
	thresh   float32
	now      nowFunc
}

// NewMemoryCache constructs an empty MemorySemanticCache. opts override the
// default Metrics, similarity threshold (0.85), and clock.
func NewMemoryCache(emb gateway.Embedder, opts ...Option) *MemorySemanticCache {
	c := newCfg(opts)
	return &MemorySemanticCache{
		embedder: emb,
		metrics:  c.metrics,
		thresh:   c.threshold,
		now:      c.now,
	}
}

// TryHit probes the cache by the embedding of signalText (§13.3.4). A row
// matches when tenantID, business, and expiresAt > now all agree AND its
// cosine similarity with the query embedding exceeds the threshold; among
// matches, the highest-similarity row wins. Returns a defensive copy on hit.
func (c *MemorySemanticCache) TryHit(ctx context.Context, tenantID int64, business, signalText string) (*strategy.Strategy, bool, error) {
	if c == nil || c.embedder == nil {
		c.recordMiss(tenantID)
		return nil, false, nil
	}
	query, err := embedOne(ctx, c.embedder, signalText)
	if err != nil {
		c.recordMiss(tenantID)
		return nil, false, err
	}

	now := c.now()
	best := float32(-1)
	var bestAdvice *strategy.Strategy

	c.mu.RLock()
	for i := range c.entries {
		e := &c.entries[i]
		if e.tenantID != tenantID || e.business != business {
			continue
		}
		if !e.expiresAt.After(now) {
			continue // expired (§7.6.4 TTL)
		}
		sim := cosineSimilarity(query, e.embedding)
		if sim > c.thresh && sim > best {
			best = sim
			// Copy the advice POINTER out of the slot while still holding the
			// lock; the entry slot itself may be rewritten by a concurrent
			// Put/Invalidate after RUnlock, but the *strategy.Strategy it
			// points at is immutable for its lifetime, so dereferencing the
			// local copy after the unlock is race-free.
			bestAdvice = e.advice
		}
	}
	c.mu.RUnlock()

	if bestAdvice == nil {
		c.recordMiss(tenantID)
		return nil, false, nil
	}
	c.metrics.IncHit(tenantID)
	return strategy.CloneStrategy(bestAdvice), true, nil
}

// Put stores a defensive copy of s under the embedding of signalText, with
// expiresAt = now + ttl. A nil s is a no-op (the pipeline calls Put after a
// successful generate, so s should always be non-nil; nil means a caller bug
// we swallow rather than panic on).
func (c *MemorySemanticCache) Put(ctx context.Context, tenantID int64, business, signalText string, s *strategy.Strategy, ttl time.Duration) error {
	if c == nil || s == nil || c.embedder == nil {
		return nil
	}
	vec, err := embedOne(ctx, c.embedder, signalText)
	if err != nil {
		return err
	}
	// Detach the vector so a caller cannot mutate the stored embedding via the
	// slice header returned by Embed (FakeEmbedder allocates fresh, but a real
	// embedder might reuse a buffer).
	emb := make([]float32, len(vec))
	copy(emb, vec)

	c.mu.Lock()
	c.entries = append(c.entries, memEntry{
		tenantID:  tenantID,
		business:  business,
		embedding: emb,
		advice:    strategy.CloneStrategy(s),
		expiresAt: c.now().Add(ttl),
	})
	c.mu.Unlock()
	return nil
}

// Invalidate drops every entry for tenantID (§7.6.4 active invalidation). The
// PGVector implementation soft-expires (UPDATE expires_at=now()); Memory just
// removes the rows since there is no shared store to leave a tombstone for.
func (c *MemorySemanticCache) Invalidate(_ context.Context, tenantID int64) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	kept := c.entries[:0]
	for _, e := range c.entries {
		if e.tenantID != tenantID {
			kept = append(kept, e)
		}
	}
	c.entries = kept
	c.mu.Unlock()
	return nil
}

// recordMiss is a nil-safe wrapper: tests occasionally construct a cache with
// nil metrics via the zero-value struct field; production always wires one.
func (c *MemorySemanticCache) recordMiss(tenantID int64) {
	if c != nil && c.metrics != nil {
		c.metrics.IncMiss(tenantID)
	}
}

// cosineSimilarity returns the cosine similarity between two equal-length
// float32 vectors (range [-1,1]; for L2-normalised embeddings this equals the
// dot product). float64 accumulation keeps the math stable for dim=1024.
// Returns 0 for length mismatch or a zero vector (no false hit).
func cosineSimilarity(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		ai := float64(a[i])
		bi := float64(b[i])
		dot += ai * bi
		na += ai * ai
		nb += bi * bi
	}
	denom := math.Sqrt(na) * math.Sqrt(nb)
	if denom == 0 {
		return 0
	}
	return float32(dot / denom)
}
