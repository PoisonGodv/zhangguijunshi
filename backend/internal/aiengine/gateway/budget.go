package gateway

// Per-tenant monthly AI cost accounting (tech plan §7.8.4). The Budget is an
// interface with memory + redis implementations for the same reason the JWT
// SessionStore is: local dev and tests have no Redis, so a stdlib impl keeps
// the circuit-breaker exercisable everywhere. Production uses Redis so the
// monthly counter survives restarts and is visible to all replicas.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Budget tracks per-tenant monthly AI spend and answers the circuit-breaker
// question: has this tenant exceeded its ceiling?
type Budget interface {
	AddCost(ctx context.Context, tenantID int64, cny float64) error
	Spent(ctx context.Context, tenantID int64) (float64, error)
	Exceeded(ctx context.Context, tenantID int64) (bool, error)
	Close() error
}

// MonthlyLimitCNY is the plan-book ceiling (§6.7 / §7.8.4): when a tenant's
// monthly AI cost reaches this, every subsequent LLM call is skipped and the
// pipeline falls back to rules + cache.
const MonthlyLimitCNY = 2.0

// costKey builds the Redis key for a tenant's current-month spend bucket.
func costKey(tenantID int64) string {
	return fmt.Sprintf("ai:cost:%d:%s", tenantID, currentYearMonth())
}

// currentYearMonth returns "YYYYMM" for now. Extracted so tests can pin via a
// (test-only) clock if needed.
func currentYearMonth() string {
	t := time.Now()
	return strconv.Itoa(t.Year()) + fmt.Sprintf("%02d", int(t.Month()))
}

// ---- memory ----

// MemoryBudget is the in-memory Budget (dev / tests).
type MemoryBudget struct {
	mu    sync.Mutex
	month string
	spent map[int64]float64
	limit float64
}

// NewMemoryBudget constructs a memory Budget; limit<=0 defaults to MonthlyLimitCNY.
func NewMemoryBudget(limit float64) *MemoryBudget {
	if limit <= 0 {
		limit = MonthlyLimitCNY
	}
	return &MemoryBudget{spent: make(map[int64]float64), limit: limit}
}

func (b *MemoryBudget) rollover() {
	m := currentYearMonth()
	if b.month != m {
		b.month = m
		b.spent = make(map[int64]float64)
	}
}

// AddCost implements Budget.
func (b *MemoryBudget) AddCost(_ context.Context, tenantID int64, cny float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	b.spent[tenantID] += cny
	return nil
}

// Spent implements Budget.
func (b *MemoryBudget) Spent(_ context.Context, tenantID int64) (float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover()
	return b.spent[tenantID], nil
}

// Exceeded implements Budget.
func (b *MemoryBudget) Exceeded(ctx context.Context, tenantID int64) (bool, error) {
	v, err := b.Spent(ctx, tenantID)
	if err != nil {
		return false, err
	}
	return v >= b.limit, nil
}

// Close implements Budget (no-op).
func (b *MemoryBudget) Close() error { return nil }

// ---- redis ----

// RedisBudget is the Redis-backed Budget.
type RedisBudget struct {
	c     redis.Cmdable
	limit float64
}

// NewRedisBudget wraps a redis.Cmdable (typically *redis.Client).
func NewRedisBudget(c redis.Cmdable, limit float64) *RedisBudget {
	if limit <= 0 {
		limit = MonthlyLimitCNY
	}
	return &RedisBudget{c: c, limit: limit}
}

// AddCost implements Budget. The bucket key expires after 40 days so stale
// counters do not accumulate.
func (b *RedisBudget) AddCost(ctx context.Context, tenantID int64, cny float64) error {
	k := costKey(tenantID)
	if err := b.c.IncrByFloat(ctx, k, cny).Err(); err != nil {
		return fmt.Errorf("gateway: budget add: %w", err)
	}
	_ = b.c.Expire(ctx, k, 40*24*time.Hour).Err()
	return nil
}

// Spent implements Budget.
func (b *RedisBudget) Spent(ctx context.Context, tenantID int64) (float64, error) {
	v, err := b.c.Get(ctx, costKey(tenantID)).Float64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("gateway: budget spent: %w", err)
	}
	return v, nil
}

// Exceeded implements Budget.
func (b *RedisBudget) Exceeded(ctx context.Context, tenantID int64) (bool, error) {
	v, err := b.Spent(ctx, tenantID)
	if err != nil {
		return false, err
	}
	return v >= b.limit, nil
}

// Close implements Budget (no-op; caller owns the client).
func (b *RedisBudget) Close() error { return nil }
