package cache

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/internal/strategy"
)

func ctx() context.Context { return context.Background() }

// sampleStrategy builds a minimal *strategy.Strategy with one item + maps +
// Signals bytes, so defensive-copy tests have something non-trivial to clone.
func sampleStrategy(id int64) *strategy.Strategy {
	return &strategy.Strategy{
		ID:                  id,
		TenantID:            5001,
		StoreID:             7001,
		ForDate:             "2026-07-28",
		Signals:             json.RawMessage(`{"business":"fruit"}`),
		DataQualityScore:    72,
		GeneratedBy:         strategy.GenLiveLLM,
		TotalCostMicroCents: 100,
		Status:              strategy.StatusGenerated,
		Items: []strategy.StrategyItem{
			{
				ID: id*10 + 1, Rank: 1, TenantID: 5001, StrategyID: id,
				ActionType: "group_seckill", ReasonType: "clearance_coupon",
				Params:         map[string]any{"discount_permille": int64(700)},
				ExpectedEffect: map[string]any{"redeem": 35},
				MarginOK:       true, ExecutionStatus: strategy.ItemPending,
			},
		},
		CreatedAt: time.Now(),
	}
}

func TestMemoryCache_PutThenTryHit(t *testing.T) {
	c := NewMemoryCache()
	s := sampleStrategy(1)
	c.Put(ctx(), 5001, 7001, "2026-07-28", s)

	got, ok := c.TryHit(ctx(), 5001, 7001, "2026-07-28")
	if !ok {
		t.Fatal("miss after Put")
	}
	if got.ID != s.ID || len(got.Items) != 1 {
		t.Fatalf("got = %+v", got)
	}
	if string(got.Signals) != string(s.Signals) {
		t.Fatalf("Signals mismatch: %s vs %s", got.Signals, s.Signals)
	}
}

func TestMemoryCache_Miss(t *testing.T) {
	c := NewMemoryCache()
	if _, ok := c.TryHit(ctx(), 5001, 7001, "2026-07-28"); ok {
		t.Fatal("hit on empty cache")
	}
}

func TestMemoryCache_KeyIsolation(t *testing.T) {
	c := NewMemoryCache()
	c.Put(ctx(), 5001, 7001, "2026-07-28", sampleStrategy(1))

	// Different tenant -> miss.
	if _, ok := c.TryHit(ctx(), 5002, 7001, "2026-07-28"); ok {
		t.Fatal("cross-tenant leak (tenant)")
	}
	// Different store -> miss.
	if _, ok := c.TryHit(ctx(), 5001, 7002, "2026-07-28"); ok {
		t.Fatal("cross-tenant leak (store)")
	}
	// Different date -> miss.
	if _, ok := c.TryHit(ctx(), 5001, 7001, "2026-07-29"); ok {
		t.Fatal("cross-tenant leak (date)")
	}
}

func TestMemoryCache_Overwrite(t *testing.T) {
	c := NewMemoryCache()
	c.Put(ctx(), 5001, 7001, "2026-07-28", sampleStrategy(1))
	c.Put(ctx(), 5001, 7001, "2026-07-28", sampleStrategy(2))
	got, ok := c.TryHit(ctx(), 5001, 7001, "2026-07-28")
	if !ok || got.ID != 2 {
		t.Fatalf("overwrite failed: %+v ok=%v", got, ok)
	}
}

func TestMemoryCache_PutNilNoop(t *testing.T) {
	c := NewMemoryCache()
	c.Put(ctx(), 5001, 7001, "2026-07-28", nil)
	if _, ok := c.TryHit(ctx(), 5001, 7001, "2026-07-28"); ok {
		t.Fatal("nil Put should not create an entry")
	}
}

// Defensive copy: mutating the source after Put, or the returned value, must
// not leak into / out of the cache.
func TestMemoryCache_DefensiveCopy(t *testing.T) {
	c := NewMemoryCache()
	src := sampleStrategy(1)
	c.Put(ctx(), 5001, 7001, "2026-07-28", src)

	// Mutate source after Put.
	src.Items[0].Copy = "SOURCE-MUT"
	src.Signals = json.RawMessage(`{"x":1}`)
	got, _ := c.TryHit(ctx(), 5001, 7001, "2026-07-28")
	if got.Items[0].Copy == "SOURCE-MUT" {
		t.Fatal("source mutation leaked into cache (item)")
	}
	if string(got.Signals) != `{"business":"fruit"}` {
		t.Fatalf("source Signals mutation leaked: %s", got.Signals)
	}

	// Mutate returned value; cache unaffected.
	got.Items[0].Copy = "RETURNED-MUT"
	got.Items[0].Params["injected"] = true
	again, _ := c.TryHit(ctx(), 5001, 7001, "2026-07-28")
	if again.Items[0].Copy == "RETURNED-MUT" {
		t.Fatal("returned-value mutation leaked back into cache")
	}
	if _, ok := again.Items[0].Params["injected"]; ok {
		t.Fatal("returned Params map shared with cache")
	}
}

func TestMemoryCache_Concurrent(t *testing.T) {
	t.Parallel()
	c := NewMemoryCache()
	const n = 40
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			c.Put(ctx(), 5001, 7001, "2026-07-28", sampleStrategy(int64(i)))
		}(i)
		go func() {
			defer wg.Done()
			c.TryHit(ctx(), 5001, 7001, "2026-07-28")
		}()
	}
	wg.Wait()
	got, ok := c.TryHit(ctx(), 5001, 7001, "2026-07-28")
	if !ok {
		t.Fatal("miss after concurrent puts")
	}
	if len(got.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.Items))
	}
}
