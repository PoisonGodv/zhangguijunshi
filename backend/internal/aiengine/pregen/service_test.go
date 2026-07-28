package pregen

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// === fakes ===

// fakePipeline implements PipelineRunner. It records per-tenant call counts,
// can fail the first failN calls per tenant (then succeed), supports alwaysErr,
// and tracks max observed concurrency to verify the worker-pool cap.
type fakePipeline struct {
	mu        sync.Mutex
	failN     int // per-tenant: fail the first failN calls, then succeed
	calls     map[int64]int
	tmpl      *strategy.Strategy
	alwaysErr error
	delay     time.Duration // hold the slot to let concurrency build up

	cur  int32
	maxc int32
}

func newFakePipeline(failN int, tmpl *strategy.Strategy) *fakePipeline {
	return &fakePipeline{calls: map[int64]int{}, failN: failN, tmpl: tmpl}
}

func (f *fakePipeline) Run(ctx context.Context) (*output.Advice, error) {
	cur := atomic.AddInt32(&f.cur, 1)
	for {
		mx := atomic.LoadInt32(&f.maxc)
		if cur <= mx || atomic.CompareAndSwapInt32(&f.maxc, mx, cur) {
			break
		}
	}
	defer atomic.AddInt32(&f.cur, -1)

	// Emulate pipeline.EnforceTenant: the tenant MUST come from ctx, never
	// trusted from a payload. pregen must inject it.
	mc, ok := multitenancy.FromContext(ctx)
	if !ok || mc.TenantID == 0 {
		return nil, multitenancy.ErrNoTenant
	}
	tid := mc.TenantID

	f.mu.Lock()
	f.calls[tid]++
	n := f.calls[tid]
	failN := f.failN
	err := f.alwaysErr
	f.mu.Unlock()

	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if err != nil {
		return nil, err
	}
	if n <= failN {
		return nil, errors.New("fake pipeline transient error")
	}
	cp := *f.tmpl // shallow copy is enough for the test
	cp.TenantID = tid
	return &output.Advice{Strategy: &cp}, nil
}

func (f *fakePipeline) callCount(tid int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[tid]
}

func (f *fakePipeline) maxConc() int32 { return atomic.LoadInt32(&f.maxc) }

type cachePutRec struct {
	tenantID, storeID int64
	date              string
}

type fakeCache struct {
	mu   sync.Mutex
	puts []cachePutRec
}

func (f *fakeCache) TryHit(ctx context.Context, tenantID, storeID int64, date string) (*strategy.Strategy, bool) {
	return nil, false
}

func (f *fakeCache) Put(ctx context.Context, tenantID, storeID int64, date string, s *strategy.Strategy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, cachePutRec{tenantID, storeID, date})
}

func (f *fakeCache) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.puts) }

type semPutRec struct {
	tenantID             int64
	business, signalText string
	ttl                  time.Duration
}

type fakeSemantic struct {
	mu   sync.Mutex
	puts []semPutRec
}

func (f *fakeSemantic) TryHit(ctx context.Context, tenantID int64, business, signalText string) (*strategy.Strategy, bool, error) {
	return nil, false, nil
}

func (f *fakeSemantic) Put(ctx context.Context, tenantID int64, business, signalText string, s *strategy.Strategy, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, semPutRec{tenantID, business, signalText, ttl})
	return nil
}

func (f *fakeSemantic) Invalidate(ctx context.Context, tenantID int64) error { return nil }

func (f *fakeSemantic) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.puts) }

type fakeEnum struct {
	ids []int64
	err error
}

func (f *fakeEnum) ActiveTenantIDs(ctx context.Context) ([]int64, error) {
	return f.ids, f.err
}

// === helpers ===

func testTemplate() *strategy.Strategy {
	bundle := signal.Bundle{
		TenantID:     0, // pipeline sets this per-tenant
		StoreID:      0,
		BusinessType: "fruit",
		Weather:      signal.Weather{Condition: "rain"},
	}
	raw, _ := json.Marshal(bundle)
	return &strategy.Strategy{
		StoreID: 7,
		ForDate: "2026-07-28",
		Signals: raw,
	}
}

func intRange(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(1000 + i)
	}
	return out
}

// fastBackoffs keeps retry tests snappy (real default is 1s/2s/4s).
var fastBackoffs = []time.Duration{
	time.Millisecond, time.Millisecond, time.Millisecond,
}

// === BatchGenerate tests ===

func TestBatchGenerate_HappyPath(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fc := &fakeCache{}
	fs := &fakeSemantic{}
	fe := &fakeEnum{ids: intRange(100)}
	s := NewScheduler(fp, fc, fs, fe,
		WithBackoffs(fastBackoffs),
		WithBatchSize(50),
		WithWorkers(8),
	)

	n, err := s.BatchGenerate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 100, n)
	assert.Equal(t, 100, fc.count(), "L1 Put per tenant")
	assert.Equal(t, 100, fs.count(), "L2 Put per tenant")

	// Concurrency makes the Put order non-deterministic, so verify the SET of
	// tenants touched plus uniform store/date/business/signalText across all
	// 100 puts rather than asserting on puts[0].
	fc.mu.Lock()
	seen := make(map[int64]cachePutRec, len(fc.puts))
	for _, r := range fc.puts {
		seen[r.tenantID] = r
		assert.Equal(t, int64(7), r.storeID, "store id from strategy template")
		assert.Equal(t, "2026-07-28", r.date, "date from strategy template")
	}
	fc.mu.Unlock()
	assert.Len(t, seen, 100, "100 distinct tenants got an L1 Put")

	fs.mu.Lock()
	assert.Len(t, fs.puts, 100)
	for _, r := range fs.puts {
		assert.Equal(t, "fruit", r.business, "business rebuilt from Signals snapshot")
		assert.Contains(t, r.signalText, "fruit|weather=rain", "signal text from snapshot")
		assert.Equal(t, semanticCacheTTL, r.ttl, "24h TTL")
	}
	fs.mu.Unlock()
}

func TestBatchGenerate_ConcurrencyCappedAtWorkers(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fp.delay = 4 * time.Millisecond // hold slots so concurrency is observable
	fc := &fakeCache{}
	fs := &fakeSemantic{}
	fe := &fakeEnum{ids: intRange(100)}
	s := NewScheduler(fp, fc, fs, fe,
		WithBackoffs(fastBackoffs),
		WithBatchSize(50),
		WithWorkers(8),
	)

	n, err := s.BatchGenerate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 100, n)
	assert.Greater(t, fp.maxConc(), int32(1), "pipeline ran concurrently")
	assert.LessOrEqual(t, fp.maxConc(), int32(8), "worker pool capped concurrency at 8")
}

func TestBatchGenerate_BatchSize50_TwoBatches(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fc := &fakeCache{}
	fs := &fakeSemantic{}
	fe := &fakeEnum{ids: intRange(100)}
	s := NewScheduler(fp, fc, fs, fe,
		WithBackoffs(fastBackoffs),
		WithBatchSize(50),
		WithWorkers(50), // workers > batch size → a batch's concurrency ceiling IS batchSize
	)

	n, err := s.BatchGenerate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 100, n)
	// Each 50-tenant batch runs sequentially; within a batch up to 50 concurrent.
	// So observed concurrency is bounded by batch size (50), never the full 100.
	assert.LessOrEqual(t, fp.maxConc(), int32(50), "batch size bounds concurrency")
}

func TestBatchGenerate_RetryThenSucceed(t *testing.T) {
	fp := newFakePipeline(2, testTemplate()) // fail twice per tenant, succeed on 3rd call
	fc := &fakeCache{}
	fs := &fakeSemantic{}
	fe := &fakeEnum{ids: []int64{5001}}
	s := NewScheduler(fp, fc, fs, fe, WithBackoffs(fastBackoffs))

	n, err := s.BatchGenerate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 3, fp.callCount(5001), "1 initial attempt + 2 retries")
	assert.Equal(t, 1, fc.count())
}

func TestBatchGenerate_RetryExhausted(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fp.alwaysErr = errors.New("permanent boom")
	fc := &fakeCache{}
	fs := &fakeSemantic{}
	fe := &fakeEnum{ids: []int64{5002}}
	s := NewScheduler(fp, fc, fs, fe, WithBackoffs(fastBackoffs))

	n, err := s.BatchGenerate(context.Background())
	require.Error(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, 4, fp.callCount(5002), "1 initial + ≤3 retries = 4 total attempts")
	assert.Equal(t, 0, fc.count(), "no cache write on permanent failure")
}

func TestBatchGenerate_PartialFailureStillCompletes(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fp.alwaysErr = errors.New("boom")
	fc := &fakeCache{}
	fs := &fakeSemantic{}
	// All tenants fail (alwaysErr is global): the batch must still complete
	// (not hang or panic) and surface an aggregated error with the failure ratio.
	fe := &fakeEnum{ids: []int64{5101, 5102, 5103}}
	s := NewScheduler(fp, fc, fs, fe, WithBackoffs(fastBackoffs))

	n, err := s.BatchGenerate(context.Background())
	require.Error(t, err)
	assert.Equal(t, 0, n)
	assert.Contains(t, err.Error(), "3/3 tenants failed")
}

func TestBatchGenerate_EmptyTenants(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	s := NewScheduler(fp, &fakeCache{}, &fakeSemantic{}, &fakeEnum{ids: nil})

	n, err := s.BatchGenerate(context.Background())
	require.NoError(t, err, "empty tenant list is not an error")
	assert.Equal(t, 0, n)
}

func TestBatchGenerate_EnumerateError(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fe := &fakeEnum{ids: nil, err: errors.New("db down")}
	s := NewScheduler(fp, &fakeCache{}, &fakeSemantic{}, fe)

	n, err := s.BatchGenerate(context.Background())
	require.Error(t, err)
	assert.Equal(t, 0, n)
	assert.Contains(t, err.Error(), "enumerate tenants")
}

func TestBatchGenerate_SemanticNil_SkipsL2(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fc := &fakeCache{}
	fe := &fakeEnum{ids: []int64{9001}}
	// sem == nil → L2 skipped, L1 still filled (dev-friendly).
	s := NewScheduler(fp, fc, nil, fe, WithBackoffs(fastBackoffs))

	n, err := s.BatchGenerate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, fc.count())
}

func TestBatchGenerate_NilCache_SkipsL1(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fs := &fakeSemantic{}
	fe := &fakeEnum{ids: []int64{9002}}
	// ch == nil → L1 skipped, L2 still filled.
	s := NewScheduler(fp, nil, fs, fe, WithBackoffs(fastBackoffs))

	n, err := s.BatchGenerate(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, fs.count())
}

// === IncrementalRetry tests ===

func TestIncrementalRetry_Drain(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fc := &fakeCache{}
	fs := &fakeSemantic{}
	s := NewScheduler(fp, fc, fs, &fakeEnum{})

	for _, tid := range []int64{7001, 7002, 7003} {
		s.EnqueueIncremental(IncrementalJob{TenantID: tid, Reason: "inventory_change"})
	}

	n, err := s.IncrementalRetry(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, 3, fc.count())
	assert.Equal(t, 3, fs.count())

	// Second drain: channel empty → 0 processed, no error.
	n2, err := s.IncrementalRetry(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n2)
}

func TestIncrementalRetry_FailureSingleAttempt(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fp.alwaysErr = errors.New("boom")
	fc := &fakeCache{}
	s := NewScheduler(fp, fc, &fakeSemantic{}, &fakeEnum{})
	s.EnqueueIncremental(IncrementalJob{TenantID: 7007, Reason: "weather_change"})

	n, err := s.IncrementalRetry(context.Background())
	require.NoError(t, err, "IncrementalRetry does NOT surface job failures as batch error")
	assert.Equal(t, 0, n)
	assert.Equal(t, 1, fp.callCount(7007), "single attempt — no within-tick retry")
	assert.Equal(t, 0, fc.count())
}

func TestIncrementalRetry_OverwritesCache(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	fc := &fakeCache{}
	fs := &fakeSemantic{}
	s := NewScheduler(fp, fc, fs, &fakeEnum{})

	s.EnqueueIncremental(IncrementalJob{TenantID: 7010, Reason: "manual_refresh"})
	n1, err := s.IncrementalRetry(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n1)

	// Re-enqueue same tenant and drain again → cache.Put called again (overwrite).
	s.EnqueueIncremental(IncrementalJob{TenantID: 7010, Reason: "manual_refresh"})
	n2, err := s.IncrementalRetry(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n2)
	assert.Equal(t, 2, fc.count(), "second Put overwrote the first")
}

// === EnqueueIncremental tests ===

func TestEnqueueIncremental_NonBlockingWhenFull(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	s := NewScheduler(fp, &fakeCache{}, &fakeSemantic{}, &fakeEnum{}, WithIncrBuffer(2))

	// Fill the buffer exactly.
	s.EnqueueIncremental(IncrementalJob{TenantID: 1})
	s.EnqueueIncremental(IncrementalJob{TenantID: 2})

	// A third enqueue must not block (dropped with a warning).
	done := make(chan struct{})
	go func() {
		s.EnqueueIncremental(IncrementalJob{TenantID: 3})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("EnqueueIncremental blocked on a full channel")
	}

	// Only the two buffered jobs are drainable.
	n, err := s.IncrementalRetry(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestEnqueueIncremental_DroppedAfterStop(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	s := NewScheduler(fp, &fakeCache{}, &fakeSemantic{}, &fakeEnum{})
	s.Stop()

	require.NotPanics(t, func() {
		s.EnqueueIncremental(IncrementalJob{TenantID: 8001, Reason: "inventory_change"})
	})

	n, err := s.IncrementalRetry(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "post-Stop enqueue is dropped, not drained")
}

// === Stop / Start lifecycle tests ===

func TestStop_ClosesChannelAndIsIdempotent(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	s := NewScheduler(fp, &fakeCache{}, &fakeSemantic{}, &fakeEnum{})

	require.NotPanics(t, func() { s.Stop() })
	require.NotPanics(t, func() { s.Stop() }, "double Stop is safe")

	// Enqueue after Stop: dropped, no panic.
	require.NotPanics(t, func() {
		s.EnqueueIncremental(IncrementalJob{TenantID: 8002})
	})

	// Drain after Stop: returns cleanly (closed channel → ok=false → return).
	n, err := s.IncrementalRetry(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestStart_Idempotent(t *testing.T) {
	fp := newFakePipeline(0, testTemplate())
	s := NewScheduler(fp, &fakeCache{}, &fakeSemantic{}, &fakeEnum{})

	require.NoError(t, s.Start())
	require.NoError(t, s.Start(), "second Start is a no-op")
	s.Stop()
}

// === deriveSemanticKey edge cases ===

func TestDeriveSemanticKey_EmptySignals(t *testing.T) {
	s := NewScheduler(newFakePipeline(0, testTemplate()), &fakeCache{}, &fakeSemantic{}, &fakeEnum{})
	business, signalText := s.deriveSemanticKey(nil)
	assert.Equal(t, "unknown", business)
	assert.Empty(t, signalText)
}

func TestDeriveSemanticKey_GarbageSignals(t *testing.T) {
	s := NewScheduler(newFakePipeline(0, testTemplate()), &fakeCache{}, &fakeSemantic{}, &fakeEnum{})
	business, signalText := s.deriveSemanticKey(json.RawMessage(`{not-json`))
	assert.Equal(t, "unknown", business)
	assert.Empty(t, signalText)
}

func TestDeriveSemanticKey_FullBundle(t *testing.T) {
	s := NewScheduler(newFakePipeline(0, testTemplate()), &fakeCache{}, &fakeSemantic{}, &fakeEnum{})
	bundle := signal.Bundle{
		BusinessType: "FRESH ", // trimmed + lowercased
		Weather:      signal.Weather{Condition: "snow"},
	}
	raw, _ := json.Marshal(bundle)
	business, signalText := s.deriveSemanticKey(raw)
	assert.Equal(t, "fresh", business)
	assert.Contains(t, signalText, "fresh|weather=snow")
}

// === chunkInt64 unit test ===

func TestChunkInt64(t *testing.T) {
	assert.Equal(t, [][]int64{}, chunkInt64(nil, 50))
	assert.Equal(t, [][]int64{}, chunkInt64([]int64{}, 50))
	assert.Equal(t, [][]int64{{1, 2, 3}}, chunkInt64([]int64{1, 2, 3}, 50))
	assert.Equal(t, [][]int64{{1, 2}, {3}}, chunkInt64([]int64{1, 2, 3}, 2))
	assert.Equal(t, [][]int64{{1}, {2}, {3}}, chunkInt64([]int64{1, 2, 3}, 1))
	// n<=0 → single chunk (no chunking).
	assert.Equal(t, [][]int64{{1, 2, 3}}, chunkInt64([]int64{1, 2, 3}, 0))
}
