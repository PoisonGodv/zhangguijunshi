package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pgvector/pgvector-go"
	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// ---- test helpers ----

func testCtx() context.Context { return context.Background() }

// sampleStrategy builds a non-trivial *strategy.Strategy (maps + Signals bytes
// + one item) so defensive-copy and JSON round-trip tests have real payload.
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
		CreatedAt: time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC),
	}
}

// ---- test embedders ----

// constantEmbedder returns the same unit basis vector (axis 0) for every text.
// Two different signalTexts therefore have cosine similarity exactly 1.0 —
// used to prove the similarity threshold fires above 0.85.
type constantEmbedder struct{ dim int }

func (e constantEmbedder) Dim() int { return e.dim }
func (e constantEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	v := make([]float32, e.dim)
	v[0] = 1 // unit basis → cosine with itself == 1
	for i := range out {
		out[i] = v
	}
	return out, nil
}

// orthogonalEmbedder maps each distinct text to a unique basis vector, so two
// different texts have cosine similarity exactly 0 (orthogonal). Same text →
// same axis → cosine 1.0. Deterministic; safe under -race.
type orthogonalEmbedder struct {
	dim  int
	mu   sync.Mutex
	seen map[string]int
	next int
}

func newOrthogonalEmbedder(dim int) *orthogonalEmbedder {
	return &orthogonalEmbedder{dim: dim, seen: make(map[string]int)}
}

func (e *orthogonalEmbedder) Dim() int { return e.dim }
func (e *orthogonalEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		axis, ok := e.seen[t]
		if !ok {
			axis = e.next
			e.next++
			e.seen[t] = axis
		}
		v := make([]float32, e.dim)
		if axis < e.dim {
			v[axis] = 1
		}
		out[i] = v
	}
	return out, nil
}

// ---- BuildSignalText tests ----

func TestBuildSignalText_FullBundle(t *testing.T) {
	b := &signal.Bundle{
		BusinessType: "fruit",
		Weather:      signal.Weather{Condition: "rain"},
		Inventory: []signal.InventorySig{
			{ProductID: 1, DaysToExpiry: 3},
			{ProductID: 2, DaysToExpiry: 2},  // min
			{ProductID: 3, DaysToExpiry: -1}, // not nearing expiry, excluded
		},
		Members: signal.MemberStats{Sleep30: 18},
		Holiday: signal.Holiday{Name: "无", DaysTo: -1, IsFestival: false},
	}
	got := BuildSignalText(b)
	want := "fruit|weather=rain|days_to_expiry=2|sleep_30=18|holiday=none"
	if got != want {
		t.Fatalf("BuildSignalText = %q, want %q", got, want)
	}
}

func TestBuildSignalText_WithHoliday(t *testing.T) {
	b := &signal.Bundle{
		BusinessType: "maternity",
		Weather:      signal.Weather{Condition: "normal"},
		Inventory:    nil,
		Members:      signal.MemberStats{Sleep30: 5},
		Holiday:      signal.Holiday{Name: "母亲节", DaysTo: 2, IsFestival: true},
	}
	got := BuildSignalText(b)
	want := "maternity|weather=normal|days_to_expiry=none|sleep_30=5|holiday=母亲节"
	if got != want {
		t.Fatalf("BuildSignalText = %q, want %q", got, want)
	}
}

func TestBuildSignalText_NilBundle(t *testing.T) {
	if got := BuildSignalText(nil); got != "" {
		t.Fatalf("BuildSignalText(nil) = %q, want empty", got)
	}
}

func TestBuildSignalText_EmptyFields(t *testing.T) {
	b := &signal.Bundle{}
	got := BuildSignalText(b)
	want := "unknown|weather=none|days_to_expiry=none|sleep_30=0|holiday=none"
	if got != want {
		t.Fatalf("BuildSignalText zero = %q, want %q", got, want)
	}
}

// ---- M8-A (§21.3): sales dimension in the semantic key ----

func TestBuildSignalText_WithSales(t *testing.T) {
	b := &signal.Bundle{
		BusinessType: "fruit",
		Weather:      signal.Weather{Condition: "rain"},
		Members:      signal.MemberStats{Sleep30: 18},
		Sales: []signal.SalesSig{
			{ProductID: 9, Trend: signal.TrendDown, SlowMoving: true},
			{ProductID: 1, Trend: signal.TrendUp, SlowMoving: true},
			{ProductID: 5, Trend: signal.TrendFlat, SlowMoving: true},
			{ProductID: 3, Trend: signal.TrendUp, BestSeller: true},
			{ProductID: 7, Trend: signal.TrendUp},
		},
	}
	got := BuildSignalText(b)
	// slow set sorted ascending (1,5,9); best set (3); up=3,down=1,flat=1.
	want := "fruit|weather=rain|days_to_expiry=none|sleep_30=18|holiday=none" +
		"|sales=slow:1,5,9;best:3;up:3,down:1,flat:1"
	if got != want {
		t.Fatalf("BuildSignalText = %q, want %q", got, want)
	}
}

func TestBuildSignalText_SalesChangesKey(t *testing.T) {
	base := &signal.Bundle{
		BusinessType: "fruit",
		Weather:      signal.Weather{Condition: "rain"},
		Members:      signal.MemberStats{Sleep30: 18},
	}
	noSales := BuildSignalText(base)

	withSales := &signal.Bundle{
		BusinessType: "fruit",
		Weather:      signal.Weather{Condition: "rain"},
		Members:      signal.MemberStats{Sleep30: 18},
		Sales: []signal.SalesSig{
			{ProductID: 1, Trend: signal.TrendUp, SlowMoving: true},
		},
	}
	if got := BuildSignalText(withSales); got == noSales {
		t.Fatalf("sales signal must change the key; both = %q", got)
	}
}

func TestBuildSignalText_NoSalesKeyUnchanged(t *testing.T) {
	// Backward compatibility: nil and empty Sales must NOT alter the legacy key.
	legacy := "fruit|weather=rain|days_to_expiry=none|sleep_30=18|holiday=none"
	for _, sales := range [][]signal.SalesSig{nil, {}} {
		b := &signal.Bundle{
			BusinessType: "fruit",
			Weather:      signal.Weather{Condition: "rain"},
			Members:      signal.MemberStats{Sleep30: 18},
			Sales:        sales,
		}
		if got := BuildSignalText(b); got != legacy {
			t.Fatalf("no-sales key changed: got %q, want legacy %q (sales=%v)", got, legacy, sales)
		}
	}
}

func TestBuildSignalText_SalesKeyStable(t *testing.T) {
	// Same logical sales set in a different slice order must yield the same key
	// (sets are sorted; trend counts are order-independent).
	a := []signal.SalesSig{
		{ProductID: 5, Trend: signal.TrendFlat, SlowMoving: true},
		{ProductID: 1, Trend: signal.TrendUp, SlowMoving: true},
		{ProductID: 3, Trend: signal.TrendUp, BestSeller: true},
	}
	b := []signal.SalesSig{
		{ProductID: 3, Trend: signal.TrendUp, BestSeller: true},
		{ProductID: 1, Trend: signal.TrendUp, SlowMoving: true},
		{ProductID: 5, Trend: signal.TrendFlat, SlowMoving: true},
	}
	ba := &signal.Bundle{BusinessType: "fruit", Sales: a}
	bb := &signal.Bundle{BusinessType: "fruit", Sales: b}
	if ka, kb := BuildSignalText(ba), BuildSignalText(bb); ka != kb {
		t.Fatalf("key not stable across input order: %q vs %q", ka, kb)
	}
	// Determinism: identical input → identical key on repeated calls.
	if k1, k2 := BuildSignalText(ba), BuildSignalText(ba); k1 != k2 {
		t.Fatalf("key not deterministic: %q vs %q", k1, k2)
	}
}

func TestBuildSignalText_SalesJitterDoesNotChangeKey(t *testing.T) {
	// Exact quantities jitter day-to-day; the key must depend only on the
	// set/bucket-level summary, so two bundles that differ only in TotalQty /
	// RecentQty / PriorQty / ActiveDays (same slow/best sets + trend buckets)
	// collapse onto one key — otherwise the cache would never hit.
	mk := func(total, recent, prior int64, days int) *signal.Bundle {
		return &signal.Bundle{
			BusinessType: "fruit",
			Sales: []signal.SalesSig{
				{ProductID: 1, TotalQty: total, RecentQty: recent, PriorQty: prior,
					ActiveDays: days, Trend: signal.TrendUp, SlowMoving: true},
				{ProductID: 2, TotalQty: total + 100, Trend: signal.TrendDown, BestSeller: true},
			},
		}
	}
	if ka, kb := BuildSignalText(mk(3, 2, 1, 4)), BuildSignalText(mk(50, 40, 10, 12)); ka != kb {
		t.Fatalf("quantity jitter must not change the key: %q vs %q", ka, kb)
	}
}

// ---- Metrics tests ----

func TestNewMetrics_IncHitMiss(t *testing.T) {
	m := NewMetrics()
	cm, ok := m.(Counters)
	if !ok {
		t.Fatal("NewMetrics() result must satisfy Counters")
	}
	if cm.Hits() != 0 || cm.Misses() != 0 {
		t.Fatalf("fresh metrics: hits=%d misses=%d, want 0/0", cm.Hits(), cm.Misses())
	}
	m.IncHit(7)
	m.IncHit(7)
	m.IncMiss(7)
	if cm.Hits() != 2 {
		t.Fatalf("after 2 IncHit: hits=%d, want 2", cm.Hits())
	}
	if cm.Misses() != 1 {
		t.Fatalf("after 1 IncMiss: misses=%d, want 1", cm.Misses())
	}
}

func TestNewMetrics_IdempotentRegistration(t *testing.T) {
	// NewMetrics must be callable multiple times without panicking on
	// duplicate prometheus registration (tests build many caches).
	a := NewMetrics()
	b := NewMetrics()
	ca, cb := a.(Counters), b.(Counters)
	b.IncHit(1)
	if ca.Hits() != 0 {
		t.Fatalf("in-memory counters must be per-instance: ca.Hits=%d want 0", ca.Hits())
	}
	if cb.Hits() != 1 {
		t.Fatalf("cb.Hits=%d want 1", cb.Hits())
	}
}

// ---- MemorySemanticCache tests ----

func TestMemorySemanticCache_SameSignalTextHit(t *testing.T) {
	m := NewMetrics()
	emb := gateway.FakeEmbedder{}
	c := NewMemoryCache(emb, WithMetrics(m))
	s := sampleStrategy(1)
	sig := "fruit|weather=rain|days_to_expiry=2|sleep_30=18|holiday=none"

	if err := c.Put(testCtx(), 5001, 7001, "fruit", sig, s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", sig)
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if !ok {
		t.Fatal("expected hit on identical signalText")
	}
	if got == nil || got.ID != s.ID {
		t.Fatalf("got=%+v, want ID=%d", got, s.ID)
	}
	if cm := m.(Counters); cm.Hits() != 1 || cm.Misses() != 0 {
		t.Fatalf("metrics hits=%d misses=%d, want 1/0", cm.Hits(), cm.Misses())
	}
}

func TestMemorySemanticCache_SimilarSignalTextHit(t *testing.T) {
	// constantEmbedder → any two signalTexts share cosine 1.0 > 0.85 → hit
	// even though the texts differ (simulating a real embedding model where
	// "雨天临期葡萄" ≈ "雨天临期苹果").
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb)
	s := sampleStrategy(2)

	put := "fruit|weather=rain|days_to_expiry=2|sleep_30=18|holiday=none"
	qry := "fruit|weather=rain|days_to_expiry=1|sleep_30=20|holiday=none" // different dims

	if err := c.Put(testCtx(), 5001, 7001, "fruit", put, s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", qry)
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if !ok {
		t.Fatal("expected hit on similar signalText (cosine 1.0 > 0.85)")
	}
	if got == nil || got.ID != s.ID {
		t.Fatalf("got=%+v, want ID=%d", got, s.ID)
	}
}

func TestMemorySemanticCache_DissimilarMiss(t *testing.T) {
	// orthogonalEmbedder → different texts map to orthogonal axes (cosine 0)
	// so a query for a never-seen signalText misses.
	emb := newOrthogonalEmbedder(gateway.EmbedDim)
	c := NewMemoryCache(emb)
	s := sampleStrategy(3)

	if err := c.Put(testCtx(), 5001, 7001, "fruit", "fruit|weather=rain", s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "fruit|weather=hot")
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if ok {
		t.Fatalf("expected miss on orthogonal signalText, got %+v", got)
	}
}

func TestMemorySemanticCache_TTLExpired(t *testing.T) {
	now := time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)
	clock := &manualClock{t: now}
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb, WithClock(clock.now))
	s := sampleStrategy(4)

	if err := c.Put(testCtx(), 5001, 7001, "fruit", "sig", s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Advance past TTL.
	clock.advance(25 * time.Hour)
	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "sig")
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if ok {
		t.Fatalf("expected miss after TTL expiry, got %+v", got)
	}
}

func TestMemorySemanticCache_TTLJustBeforeExpiryHits(t *testing.T) {
	now := time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)
	clock := &manualClock{t: now}
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb, WithClock(clock.now))
	s := sampleStrategy(5)

	if err := c.Put(testCtx(), 5001, 7001, "fruit", "sig", s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	clock.advance(23 * time.Hour) // still within TTL
	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "sig")
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if !ok {
		t.Fatal("expected hit just before TTL expiry")
	}
	if got.ID != s.ID {
		t.Fatalf("got ID=%d, want %d", got.ID, s.ID)
	}
}

func TestMemorySemanticCache_Invalidate(t *testing.T) {
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb)
	s := sampleStrategy(6)

	if err := c.Put(testCtx(), 5001, 7001, "fruit", "sig", s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := c.Invalidate(testCtx(), 5001); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "sig")
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if ok {
		t.Fatalf("expected miss after Invalidate, got %+v", got)
	}
}

func TestMemorySemanticCache_InvalidateOnlyTargetTenant(t *testing.T) {
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb)

	if err := c.Put(testCtx(), 5001, 7001, "fruit", "sig", sampleStrategy(7), 24*time.Hour); err != nil {
		t.Fatalf("Put 5001: %v", err)
	}
	if err := c.Put(testCtx(), 5002, 7001, "fruit", "sig", sampleStrategy(8), 24*time.Hour); err != nil {
		t.Fatalf("Put 5002: %v", err)
	}
	if err := c.Invalidate(testCtx(), 5001); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "sig"); err != nil || ok {
		t.Fatalf("tenant 5001 should be invalidated; ok=%v err=%v", ok, err)
	}
	if got, ok, err := c.TryHit(testCtx(), 5002, 7001, "fruit", "sig"); err != nil || !ok {
		t.Fatalf("tenant 5002 must survive targeted invalidate; ok=%v err=%v", ok, err)
	} else if got.ID != 8 {
		t.Fatalf("tenant 5002 got ID=%d, want 8", got.ID)
	}
}

func TestMemorySemanticCache_CrossTenantMiss(t *testing.T) {
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb)
	s := sampleStrategy(9)

	if err := c.Put(testCtx(), 5001, 7001, "fruit", "sig", s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.TryHit(testCtx(), 5002, 7001, "fruit", "sig") // same text, different tenant
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if ok {
		t.Fatalf("cross-tenant must miss (ARCH-01 tenant isolation), got %+v", got)
	}
}

func TestMemorySemanticCache_CrossBusinessMiss(t *testing.T) {
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb)
	s := sampleStrategy(10)

	if err := c.Put(testCtx(), 5001, 7001, "fruit", "sig", s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fresh", "sig") // same tenant+store+text, different business
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if ok {
		t.Fatalf("cross-business must miss (§7.6.4 same-business filter), got %+v", got)
	}
}

// TestMemorySemanticCache_CrossStoreMiss is the regression test for the
// high-severity cross-store leak (aiengine 修复①): a multi-store tenant's
// near-identical signal on store B must NOT hit store A's cached draft. The
// constantEmbedder forces cosine 1.0 so ONLY the (tenant, store, business)
// filter can prevent the hit — proving store_id is in the retrieval key.
func TestMemorySemanticCache_CrossStoreMiss(t *testing.T) {
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb)
	s := sampleStrategy(12)

	// Store 7001 writes a draft under signal "sig".
	if err := c.Put(testCtx(), 5001, 7001, "fruit", "sig", s, 24*time.Hour); err != nil {
		t.Fatalf("Put store 7001: %v", err)
	}
	// Same tenant, SAME signal text + embedding, but store 7002 → must miss.
	got, ok, err := c.TryHit(testCtx(), 5001, 7002, "fruit", "sig")
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if ok {
		t.Fatalf("cross-store must miss (migration 000022 store isolation), got %+v", got)
	}
	// The owning store still hits (sanity: we did not over-restrict).
	got2, ok2, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "sig")
	if err != nil || !ok2 {
		t.Fatalf("owning store must still hit; ok=%v err=%v", ok2, err)
	}
	if got2.ID != s.ID {
		t.Fatalf("owning store got ID=%d, want %d", got2.ID, s.ID)
	}
}

func TestMemorySemanticCache_DefensiveCopy(t *testing.T) {
	emb := gateway.FakeEmbedder{}
	c := NewMemoryCache(emb)
	s := sampleStrategy(11)
	sig := "fruit|weather=rain"

	if err := c.Put(testCtx(), 5001, 7001, "fruit", sig, s, 24*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", sig)
	if err != nil || !ok {
		t.Fatalf("TryHit: ok=%v err=%v", ok, err)
	}
	// Mutate the returned copy; the stored snapshot must not change.
	got.ID = 99999
	got.DataQualityScore = 1
	got.Items[0].ActionType = "coupon_wake"
	got.Items[0].Params = map[string]any{"injected": true}
	got.Signals = json.RawMessage(`{"injected":true}`)

	got2, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", sig)
	if err != nil || !ok {
		t.Fatalf("second TryHit: ok=%v err=%v", ok, err)
	}
	if got2.ID != s.ID {
		t.Fatalf("stored ID changed: got=%d want=%d (defensive copy leaked)", got2.ID, s.ID)
	}
	if got2.Items[0].ActionType != "group_seckill" {
		t.Fatalf("stored item mutated: action_type=%q", got2.Items[0].ActionType)
	}
}

func TestMemorySemanticCache_PutNilStrategy(t *testing.T) {
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb)
	if err := c.Put(testCtx(), 5001, 7001, "fruit", "sig", nil, 24*time.Hour); err != nil {
		t.Fatalf("Put(nil) should be a no-op, got err: %v", err)
	}
	if _, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "sig"); err != nil || ok {
		t.Fatalf("after Put(nil): ok=%v err=%v", ok, err)
	}
}

func TestMemorySemanticCache_NilEmbedder(t *testing.T) {
	c := NewMemoryCache(nil)
	if _, ok, err := c.TryHit(testCtx(), 1, 0, "fruit", "sig"); err != nil || ok {
		t.Fatalf("nil embedder TryHit: ok=%v err=%v", ok, err)
	}
}

func TestMemorySemanticCache_ConcurrentAccess(t *testing.T) {
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewMemoryCache(emb)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = c.Put(testCtx(), int64(i%5), int64(i%7), "fruit", fmt.Sprintf("sig%d", i%3), sampleStrategy(int64(i)), 24*time.Hour)
			_, _, _ = c.TryHit(testCtx(), int64(i%5), int64(i%7), "fruit", "sig0")
			_ = c.Invalidate(testCtx(), int64(i%5))
		}(i)
	}
	wg.Wait() // -race guards the RWMutex
}

// ---- PGVectorSemanticCache tests (fake Pool) ----

func TestPGVectorSemanticCache_TryHit(t *testing.T) {
	advice := sampleStrategy(20)
	raw, err := json.Marshal(advice)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pool := newFakePool(&fakeRow{json: raw})
	emb := constantEmbedder{dim: gateway.EmbedDim}
	m := NewMetrics()
	c := NewPGVectorCache(pool, emb, WithMetrics(m))

	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "fruit|weather=rain")
	if err != nil {
		t.Fatalf("TryHit err: %v", err)
	}
	if !ok || got == nil {
		t.Fatalf("expected hit, ok=%v got=%+v", ok, got)
	}
	if got.ID != advice.ID {
		t.Fatalf("got.ID=%d want %d", got.ID, advice.ID)
	}
	if got.Items[0].ActionType != "group_seckill" {
		t.Fatalf("item action_type lost in JSON round-trip: %q", got.Items[0].ActionType)
	}

	// Verify the recorded SQL + args.
	if len(pool.calls) != 1 || pool.calls[0].method != "QueryRow" {
		t.Fatalf("want 1 QueryRow call, got %+v", pool.calls)
	}
	call := pool.calls[0]
	if call.sql != sqlSemanticHit {
		t.Fatalf("SQL mismatch:\ngot:  %s\nwant: %s", call.sql, sqlSemanticHit)
	}
	if len(call.args) != 4 {
		t.Fatalf("want 4 args, got %d", len(call.args))
	}
	if tid, ok := call.args[0].(int64); !ok || tid != 5001 {
		t.Fatalf("arg[0] (tenantID) = %v, want int64(5001)", call.args[0])
	}
	if sid, ok := call.args[1].(int64); !ok || sid != 7001 {
		t.Fatalf("arg[1] (storeID) = %v, want int64(7001)", call.args[1])
	}
	if biz, ok := call.args[2].(string); !ok || biz != "fruit" {
		t.Fatalf("arg[2] (business) = %v, want \"fruit\"", call.args[2])
	}
	vec, ok := call.args[3].(pgvector.Vector)
	if !ok {
		t.Fatalf("arg[3] type = %T, want pgvector.Vector", call.args[3])
	}
	if len(vec.Slice()) != gateway.EmbedDim {
		t.Fatalf("embedding dim = %d, want %d", len(vec.Slice()), gateway.EmbedDim)
	}
	if cm := m.(Counters); cm.Hits() != 1 || cm.Misses() != 0 {
		t.Fatalf("metrics hits=%d misses=%d, want 1/0", cm.Hits(), cm.Misses())
	}
}

func TestPGVectorSemanticCache_TryHitNoRows(t *testing.T) {
	pool := newFakePool(&fakeRow{err: pgx.ErrNoRows})
	c := NewPGVectorCache(pool, constantEmbedder{dim: gateway.EmbedDim})

	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "fruit|weather=rain")
	if err != nil {
		t.Fatalf("ErrNoRows must surface as miss not error: %v", err)
	}
	if ok || got != nil {
		t.Fatalf("expected miss on pgx.ErrNoRows, got ok=%v %+v", ok, got)
	}
}

func TestPGVectorSemanticCache_TryHitQueryError(t *testing.T) {
	pool := newFakePool(&fakeRow{err: errors.New("connection lost")})
	c := NewPGVectorCache(pool, constantEmbedder{dim: gateway.EmbedDim})

	got, ok, err := c.TryHit(testCtx(), 5001, 7001, "fruit", "fruit|weather=rain")
	if err == nil {
		t.Fatal("expected error from Scan failure")
	}
	if ok || got != nil {
		t.Fatalf("on Scan error: ok=%v got=%+v", ok, got)
	}
}

func TestPGVectorSemanticCache_Put(t *testing.T) {
	pool := newFakePool(nil)
	emb := constantEmbedder{dim: gateway.EmbedDim}
	c := NewPGVectorCache(pool, emb)
	s := sampleStrategy(21)

	if err := c.Put(testCtx(), 5001, 7001, "fruit", "fruit|weather=rain", s, 24*time.Hour); err != nil {
		t.Fatalf("Put err: %v", err)
	}
	if len(pool.calls) != 1 || pool.calls[0].method != "Exec" {
		t.Fatalf("want 1 Exec call, got %+v", pool.calls)
	}
	call := pool.calls[0]
	if call.sql != sqlSemanticPut {
		t.Fatalf("SQL mismatch:\ngot:  %s\nwant: %s", call.sql, sqlSemanticPut)
	}
	if len(call.args) != 7 {
		t.Fatalf("want 7 args, got %d", len(call.args))
	}
	if tid, ok := call.args[0].(int64); !ok || tid != 5001 {
		t.Fatalf("arg[0] (tenantID) = %v, want int64(5001)", call.args[0])
	}
	if sid, ok := call.args[1].(int64); !ok || sid != 7001 {
		t.Fatalf("arg[1] (storeID) = %v, want int64(7001)", call.args[1])
	}
	if biz, ok := call.args[2].(string); !ok || biz != "fruit" {
		t.Fatalf("arg[2] (business) = %v, want \"fruit\"", call.args[2])
	}
	if kt, ok := call.args[3].(string); !ok || kt != "fruit|weather=rain" {
		t.Fatalf("arg[3] (key_text) = %v, want signal text", call.args[3])
	}
	if _, ok := call.args[4].(pgvector.Vector); !ok {
		t.Fatalf("arg[4] type = %T, want pgvector.Vector", call.args[4])
	}
	// arg[5] is advice JSONB bytes.
	adviceBytes, ok := call.args[5].([]byte)
	if !ok {
		t.Fatalf("arg[5] type = %T, want []byte (advice JSON)", call.args[5])
	}
	var check strategy.Strategy
	if err := json.Unmarshal(adviceBytes, &check); err != nil {
		t.Fatalf("advice JSON did not round-trip: %v", err)
	}
	if check.ID != s.ID {
		t.Fatalf("advice JSON ID=%d, want %d", check.ID, s.ID)
	}
	// arg[6] is expires_at, now + ttl.
	expiresAt, ok := call.args[6].(time.Time)
	if !ok {
		t.Fatalf("arg[6] type = %T, want time.Time", call.args[6])
	}
	if expiresAt.Before(time.Now()) {
		t.Fatalf("expires_at %v should be in the future", expiresAt)
	}
}

func TestPGVectorSemanticCache_PutNilStrategy(t *testing.T) {
	pool := newFakePool(nil)
	c := NewPGVectorCache(pool, constantEmbedder{dim: gateway.EmbedDim})
	if err := c.Put(testCtx(), 1, 0, "fruit", "sig", nil, 24*time.Hour); err != nil {
		t.Fatalf("Put(nil) should be no-op, got: %v", err)
	}
	if len(pool.calls) != 0 {
		t.Fatalf("Put(nil) made %d DB calls, want 0", len(pool.calls))
	}
}

func TestPGVectorSemanticCache_Invalidate(t *testing.T) {
	pool := newFakePool(nil)
	c := NewPGVectorCache(pool, constantEmbedder{dim: gateway.EmbedDim})

	if err := c.Invalidate(testCtx(), 5001); err != nil {
		t.Fatalf("Invalidate err: %v", err)
	}
	if len(pool.calls) != 1 || pool.calls[0].method != "Exec" {
		t.Fatalf("want 1 Exec call, got %+v", pool.calls)
	}
	call := pool.calls[0]
	if call.sql != sqlSemanticInvalidate {
		t.Fatalf("SQL mismatch:\ngot:  %s\nwant: %s", call.sql, sqlSemanticInvalidate)
	}
	if len(call.args) != 1 {
		t.Fatalf("want 1 arg, got %d", len(call.args))
	}
	if tid, ok := call.args[0].(int64); !ok || tid != 5001 {
		t.Fatalf("arg[0] (tenantID) = %v, want int64(5001)", call.args[0])
	}
}

func TestPGVectorSemanticCache_NilPool(t *testing.T) {
	c := NewPGVectorCache(nil, constantEmbedder{dim: gateway.EmbedDim})
	if _, ok, err := c.TryHit(testCtx(), 1, 0, "fruit", "sig"); err != nil || ok {
		t.Fatalf("nil pool TryHit: ok=%v err=%v", ok, err)
	}
	if err := c.Put(testCtx(), 1, 0, "fruit", "sig", sampleStrategy(1), 24*time.Hour); err != nil {
		t.Fatalf("nil pool Put: %v", err)
	}
	if err := c.Invalidate(testCtx(), 1); err != nil {
		t.Fatalf("nil pool Invalidate: %v", err)
	}
}

func TestPGVectorSemanticCache_SatisfiesInterface(t *testing.T) {
	// Compile-time: both implementations satisfy SemanticCache.
	var _ SemanticCache = (*MemorySemanticCache)(nil)
	var _ SemanticCache = (*PGVectorSemanticCache)(nil)
}

// ---- fake Pool + Row ----

type fakeRow struct {
	json []byte
	err  error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 0 {
		return nil
	}
	bp, ok := dest[0].(*[]byte)
	if !ok {
		return fmt.Errorf("fakeRow.Scan: dest[0] is %T, want *[]byte", dest[0])
	}
	*bp = r.json
	return nil
}

type fakeCall struct {
	method string // "QueryRow" | "Exec"
	sql    string
	args   []any
}

type fakePool struct {
	mu     sync.Mutex
	calls  []fakeCall
	row    *fakeRow
	execOK pgconn.CommandTag
	execFn func(sql string, args []any) error
}

func newFakePool(row *fakeRow) *fakePool {
	return &fakePool{row: row, execOK: pgconn.NewCommandTag("INSERT 0 1")}
}

func (p *fakePool) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Copy args so later caller mutation does not rewrite the record.
	cp := make([]any, len(args))
	copy(cp, args)
	p.calls = append(p.calls, fakeCall{method: "QueryRow", sql: sql, args: cp})
	return p.row
}

func (p *fakePool) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]any, len(args))
	copy(cp, args)
	p.calls = append(p.calls, fakeCall{method: "Exec", sql: sql, args: cp})
	if p.execFn != nil {
		return p.execOK, p.execFn(sql, cp)
	}
	return p.execOK, nil
}

func (p *fakePool) lastCall() fakeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return fakeCall{}
	}
	return p.calls[len(p.calls)-1]
}

// ---- manualClock ----

type manualClock struct {
	t  time.Time
	mu sync.Mutex
}

func (c *manualClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
