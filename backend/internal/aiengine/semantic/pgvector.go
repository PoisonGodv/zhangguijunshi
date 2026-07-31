package semantic

// pgvector.go implements PGVectorSemanticCache — the production L2 semantic
// cache backed by PostgreSQL + pgvector. Embeddings live in the ai_cache table
// (migration 000008: vector(1024) + HNSW cosine index); retrieval uses the
// pgvector cosine-distance operator `<=>` (<0.15 == similarity >0.85, §7.6.4).
//
// The DB dependency is a narrow Pool interface (QueryRow / Exec) so tests run
// with a fake and the package does not hard-import pgxpool. In production the
// main thread injects *pgxpool.Pool (after pgvector.Register), which satisfies
// Pool; the embedding arg is a pgvector.Vector (pgvector-go), encoded via its
// driver.Valuer / pgx codec. The main thread MUST register the pgvector type
// on the pool before any real query; in tests the fake Pool captures args
// verbatim so no encoding happens.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"
	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// Pool is the narrow database surface PGVectorSemanticCache needs (CONTRACT
// §13.5.3 / task: avoid hard pgxpool import). It is exactly the subset of
// *pgxpool.Pool / pgx.Tx the cache uses — QueryRow and Exec — so the main
// thread can pass either, and tests inject a fake.
type Pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// SQL statements (§7.6.3). $4 in TryHit is a pgvector.Vector; the main thread
// registers the type on the pool so pgx sends it with the vector OID. The
// distance threshold 0.15 mirrors the Memory similarity threshold 0.85
// (distance = 1 - similarity). store_id is part of the retrieval key
// (migration 000022): a multi-store tenant's near-identical signal on a
// different store must NOT hit another store's cached draft.
const (
	// sqlSemanticHit selects the single closest non-expired advice within the
	// same tenant + store + business, cosine-distance < 0.15 (similarity > 0.85).
	sqlSemanticHit = `SELECT advice_json FROM ai_cache
WHERE tenant_id = $1 AND store_id = $2 AND business = $3 AND expires_at > now() AND embedding <=> $4 < 0.15
ORDER BY embedding <=> $4 LIMIT 1`

	// sqlSemanticPut inserts a new cache row; hit_count defaults to 0 via the
	// table DDL.
	sqlSemanticPut = `INSERT INTO ai_cache (tenant_id, store_id, business, key_text, embedding, advice_json, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

	// sqlSemanticInvalidate soft-expires every row for tenantID
	// (UPDATE expires_at=now() WHERE tenant_id=$1, §7.6.4).
	sqlSemanticInvalidate = `UPDATE ai_cache SET expires_at = now() WHERE tenant_id = $1`
)

// PGVectorSemanticCache is the production L2 semantic cache (CONTRACT §13.3.4
// / §13.5.3). It stores embeddings in ai_cache and retrieves by pgvector
// cosine distance. A nil pool is a no-op cache (dev wires MemorySemanticCache
// instead, but a defensive nil-check keeps a misconfigured main thread from
// crashing the pipeline).
type PGVectorSemanticCache struct {
	pool     Pool
	embedder gateway.Embedder
	metrics  Metrics
	now      nowFunc
}

// NewPGVectorCache constructs a PGVectorSemanticCache. opts override Metrics
// and the clock (the Memory similarity threshold does not apply — distance is
// encoded in SQL).
func NewPGVectorCache(pool Pool, emb gateway.Embedder, opts ...Option) *PGVectorSemanticCache {
	c := newCfg(opts)
	return &PGVectorSemanticCache{
		pool:     pool,
		embedder: emb,
		metrics:  c.metrics,
		now:      c.now,
	}
}

// TryHit embeds signalText and queries ai_cache for the closest non-expired
// row within (tenantID, storeID, business) at cosine distance < 0.15. On a hit
// the advice_json is unmarshalled into a fresh *strategy.Strategy (already
// independent, no extra clone needed). pgx.ErrNoRows is a miss, not an error.
func (c *PGVectorSemanticCache) TryHit(ctx context.Context, tenantID, storeID int64, business, signalText string) (*strategy.Strategy, bool, error) {
	if c == nil || c.pool == nil || c.embedder == nil {
		c.recordMiss(tenantID)
		return nil, false, nil
	}
	vec, err := embedOne(ctx, c.embedder, signalText)
	if err != nil {
		c.recordMiss(tenantID)
		return nil, false, err
	}

	var raw []byte
	err = c.pool.QueryRow(ctx, sqlSemanticHit, tenantID, storeID, business, pgvector.NewVector(vec)).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.metrics.IncMiss(tenantID)
			return nil, false, nil
		}
		c.recordMiss(tenantID)
		return nil, false, fmt.Errorf("semantic: pgvector tryhit: %w", err)
	}
	if len(raw) == 0 {
		c.metrics.IncMiss(tenantID)
		return nil, false, nil
	}

	var s strategy.Strategy
	if err := json.Unmarshal(raw, &s); err != nil {
		c.recordMiss(tenantID)
		return nil, false, fmt.Errorf("semantic: unmarshal advice_json: %w", err)
	}
	c.metrics.IncHit(tenantID)
	return &s, true, nil
}

// Put embeds signalText and INSERTs a new ai_cache row. advice_json is the
// JSON-encoded *strategy.Strategy; expires_at = now + ttl (24h from the
// pipeline). A nil s is a no-op. Returns the underlying error on failure;
// callers (the pipeline) swallow it so a failed write just means a cold run.
func (c *PGVectorSemanticCache) Put(ctx context.Context, tenantID, storeID int64, business, signalText string, s *strategy.Strategy, ttl time.Duration) error {
	if c == nil || c.pool == nil || c.embedder == nil || s == nil {
		return nil
	}
	vec, err := embedOne(ctx, c.embedder, signalText)
	if err != nil {
		return err
	}
	adviceJSON, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("semantic: marshal advice: %w", err)
	}
	expiresAt := c.now().Add(ttl)
	if _, err := c.pool.Exec(ctx, sqlSemanticPut, tenantID, storeID, business, signalText, pgvector.NewVector(vec), adviceJSON, expiresAt); err != nil {
		return fmt.Errorf("semantic: pgvector put: %w", err)
	}
	return nil
}

// Invalidate soft-expires every ai_cache row for tenantID
// (UPDATE expires_at=now() WHERE tenant_id=$1, §7.6.4). Rows remain in the
// table (for audit / hit_count) but no longer match the expires_at > now()
// predicate in TryHit.
func (c *PGVectorSemanticCache) Invalidate(ctx context.Context, tenantID int64) error {
	if c == nil || c.pool == nil {
		return nil
	}
	if _, err := c.pool.Exec(ctx, sqlSemanticInvalidate, tenantID); err != nil {
		return fmt.Errorf("semantic: pgvector invalidate: %w", err)
	}
	return nil
}

// recordMiss is a nil-safe wrapper for the early-return branches.
func (c *PGVectorSemanticCache) recordMiss(tenantID int64) {
	if c != nil && c.metrics != nil {
		c.metrics.IncMiss(tenantID)
	}
}
