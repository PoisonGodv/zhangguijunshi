package semantic

// service.go holds the cross-implementation plumbing for the L2 semantic
// cache: the functional Option type + internal cfg, the default Metrics
// implementation (prometheus + in-memory counters), and the embedOne helper
// shared by MemorySemanticCache and PGVectorSemanticCache.

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
)

// Option configures a SemanticCache constructor (Memory or PGVector). The zero
// config is sensible for production: NewMetrics(), threshold 0.85, time.Now.
type Option func(*cfg)

// cfg is the resolved configuration shared by both implementations.
type cfg struct {
	metrics   Metrics // hit/miss counters (never nil)
	threshold float32 // Memory cosine-similarity threshold (>this == hit)
	now       nowFunc // clock (never nil)
}

// newCfg builds the default cfg and applies opts. The Memory implementation
// uses threshold; PGVector encodes the equivalent (distance < 0.15) in SQL and
// ignores threshold.
func newCfg(opts []Option) cfg {
	c := cfg{
		metrics:   NewMetrics(),
		threshold: memorySimilarityThreshold, // 0.85 (§7.6.4)
		now:       defaultNow,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	if c.metrics == nil {
		c.metrics = NewMetrics()
	}
	if c.now == nil {
		c.now = defaultNow
	}
	return c
}

// WithMetrics injects a custom Metrics (e.g. a shared instance across L1/L2,
// or a fake in tests). A nil m is ignored so callers can write
// WithMetrics(maybeNil) safely.
func WithMetrics(m Metrics) Option {
	return func(c *cfg) {
		if m != nil {
			c.metrics = m
		}
	}
}

// WithThreshold overrides the Memory cosine-similarity threshold (default
// 0.85, §7.6.4). PGVector ignores this — its distance threshold (<0.15) is
// encoded in the retrieval SQL.
func WithThreshold(t float32) Option {
	return func(c *cfg) { c.threshold = t }
}

// WithClock injects a custom clock (tests advance a fake). A nil f is ignored.
func WithClock(f nowFunc) Option {
	return func(c *cfg) {
		if f != nil {
			c.now = f
		}
	}
}

// memorySimilarityThreshold is the Memory-implementation cosine-similarity
// threshold (§7.6.4 / §13.5.7): similarity > 0.85 is a hit. The PGVector
// implementation expresses the same cutoff as cosine distance < 0.15 in SQL.
const memorySimilarityThreshold float32 = 0.85

// embedOne embeds a single text via emb and validates the dimension. Both
// caches need exactly one vector per signalText; centralising the call keeps
// the error shape and dim-check uniform.
func embedOne(ctx context.Context, emb gateway.Embedder, text string) ([]float32, error) {
	if emb == nil {
		return nil, fmt.Errorf("semantic: embedder is nil")
	}
	vecs, err := emb.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("semantic: embed signal text: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("semantic: embedder returned %d vectors, want 1", len(vecs))
	}
	if got := len(vecs[0]); got != emb.Dim() {
		return nil, fmt.Errorf("semantic: embedding dim %d, want %d", got, emb.Dim())
	}
	return vecs[0], nil
}

// ---- Metrics implementation ----

// promMetrics is the default Metrics: two prometheus counter vecs
// (ai_cache_hit_total / ai_cache_miss_total, labelled by tenant id) plus
// in-memory hit/miss totals for test assertion (Counters). Safe for concurrent
// use; NewMetrics is idempotent against the default registry so multiple calls
// in one process (e.g. tests) do not panic on duplicate registration.
type promMetrics struct {
	hitTotal  *prometheus.CounterVec
	missTotal *prometheus.CounterVec
	hits      atomic.Int64
	misses    atomic.Int64
}

// NewMetrics builds the default Metrics (CONTRACT §13.3.4). It registers two
// prometheus counter vecs with the default registry, tolerating re-registration
// (tests / multiple caches in one process) by reusing the existing collector.
func NewMetrics() Metrics {
	m := &promMetrics{
		hitTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ai_cache_hit_total",
			Help: "Total number of L2 semantic cache hits (CONTRACT §13.3.4 / §7.6.5).",
		}, []string{"tenant"}),
		missTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ai_cache_miss_total",
			Help: "Total number of L2 semantic cache misses (CONTRACT §13.3.4 / §7.6.5).",
		}, []string{"tenant"}),
	}
	m.hitTotal = registerOrReuse(m.hitTotal).(*prometheus.CounterVec)
	m.missTotal = registerOrReuse(m.missTotal).(*prometheus.CounterVec)
	return m
}

// registerOrReuse registers c with the default prometheus registry, returning
// c. If a collector with the same descriptor is already registered, the
// existing collector is returned instead so NewMetrics is idempotent (no panic
// on duplicate registration — essential for tests that build many caches).
func registerOrReuse(c prometheus.Collector) prometheus.Collector {
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return are.ExistingCollector
		}
	}
	return c
}

// IncHit implements Metrics.
func (m *promMetrics) IncHit(tenantID int64) {
	m.hits.Add(1)
	m.hitTotal.WithLabelValues(tenantLabel(tenantID)).Inc()
}

// IncMiss implements Metrics.
func (m *promMetrics) IncMiss(tenantID int64) {
	m.misses.Add(1)
	m.missTotal.WithLabelValues(tenantLabel(tenantID)).Inc()
}

// Hits implements Counters (test/introspection).
func (m *promMetrics) Hits() int64 { return m.hits.Load() }

// Misses implements Counters (test/introspection).
func (m *promMetrics) Misses() int64 { return m.misses.Load() }

// tenantLabel renders tenantID as a stable prometheus label value. "system"
// (tenantID <= 0) collapses to one label so a misconfigured caller cannot
// blow up the label cardinality.
func tenantLabel(tenantID int64) string {
	if tenantID <= 0 {
		return "system"
	}
	return fmt.Sprintf("%d", tenantID)
}

// noopMetrics is a Metrics that discards everything. Kept for future use as a
// safe zero-value default; currently NewMetrics is always wired.
var _ Metrics = (*promMetrics)(nil)

// promMetrics satisfies Metrics at compile time (guard against interface drift).
var _ Metrics = (*noopMetrics)(nil)

type noopMetrics struct{}

func (noopMetrics) IncHit(int64)  {}
func (noopMetrics) IncMiss(int64) {}
