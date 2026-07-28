// Package pregen implements the offline early-morning pre-generation and
// daytime incremental re-compute of the AI engine's "today's 3 things" advice
// (CONTRACT §13.3.5, tech plan ch07 §7.7).
//
// At 03:00 every day a cron-triggered BatchGenerate walks every active tenant,
// runs the pipeline once per tenant, and writes the resulting Advice into both
// the L1 exact-hash cache (cache.Cache) and the L2 semantic cache
// (semantic.SemanticCache, TTL 24h). Through the day an in-memory channel feeds
// an IncrementalRetry job that re-runs the pipeline for tenants whose signals
// changed (inventory / weather / manual refresh / holiday rollover) and
// overwrites the cached draft. Per-tenant failures retry up to 3 times with
// exponential backoff (1s/2s/4s); the batch never aborts on a single tenant's
// error — "保证白天打开 App 有建议" (§7.7.4).
//
// The Scheduler depends on a narrow PipelineRunner interface rather than
// *aiengine.Pipeline directly, so this package compiles cleanly even while the
// pipeline constructor signature is being evolved concurrently by A5
// (CONTRACT §13.5 anti-corruption note). *aiengine.Pipeline satisfies
// PipelineRunner structurally.
package pregen

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/zhangguijunshi/backend/internal/aiengine/cache"
	"github.com/zhangguijunshi/backend/internal/aiengine/output"
	"github.com/zhangguijunshi/backend/internal/aiengine/semantic"
	"go.uber.org/zap"
)

// PipelineRunner is the narrow anti-corruption interface the Scheduler needs
// from the pipeline (CONTRACT §13.3.5 / §13.5). *aiengine.Pipeline satisfies it
// structurally. pregen does NOT depend on the pipeline's constructor signature
// so that NewPipeline can evolve concurrently without breaking this package.
type PipelineRunner interface {
	// Run produces today's advice for the tenant resolved from ctx. pregen
	// injects the per-tenant identity (Source=system) into ctx before each call.
	Run(ctx context.Context) (*output.Advice, error)
}

// TenantEnumerator is the anti-corruption port for "all active tenant ids".
// The main thread adapts the real tenant/store repository to this interface;
// tests inject a fake. It is the ONLY way pregen learns which tenants exist —
// pregen never trusts a tenant id from the pipeline output.
type TenantEnumerator interface {
	ActiveTenantIDs(ctx context.Context) ([]int64, error)
}

// IncrementalJob is one unit of daytime re-compute work (§7.7.3). Reason is
// inventory_change|weather_change|manual_refresh|holiday_rollover and is for
// observability only.
type IncrementalJob struct {
	TenantID int64
	Reason   string
}

// Cron schedule literals (CONTRACT §13.3.5). Standard 5-field specs parsed by
// robfig/cron/v3's default parser.
const (
	batchCronSpec       = "0 3 * * *"    // 03:00 daily, full pre-generation
	incrementalCronSpec = "*/10 * * * *" // every 10 min, drain incremental queue
)

// Tunable defaults (CONTRACT §13.3.5 / §7.7.2).
const (
	defaultBatchSize  = 50             // tenants per batch
	defaultWorkers    = 8              // concurrent pipeline runs (LLM RPM cap)
	defaultIncrBuffer = 256            // buffered incremental jobs
	semanticCacheTTL  = 24 * time.Hour // L2 draft validity (§7.6.4)
)

// defaultBackoffs is the exponential retry schedule (§7.7.4): wait 1s before
// retry 1, 2s before retry 2, 4s before retry 3. The slice length IS the max
// retry count (3); the initial attempt is not counted as a retry.
var defaultBackoffs = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}

// Scheduler owns the pre-generation cron and the incremental-job channel. It is
// safe for concurrent use after NewScheduler.
type Scheduler struct {
	pipeline PipelineRunner
	ch       cache.Cache
	sem      semantic.SemanticCache
	repo     TenantEnumerator
	log      *zap.Logger
	cron     *cron.Cron
	incr     chan IncrementalJob
	now      func() time.Time

	batchSize  int
	workers    int
	backoffs   []time.Duration
	incrBuffer int
	location   *time.Location

	mu      sync.Mutex
	started bool
	closed  bool
}

// Option configures a Scheduler.
type Option func(*Scheduler)

// WithLogger overrides the default no-op logger.
func WithLogger(l *zap.Logger) Option {
	return func(s *Scheduler) {
		if l != nil {
			s.log = l
		}
	}
}

// WithNowFunc overrides the clock (deterministic run timestamps in tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Scheduler) {
		if f != nil {
			s.now = f
		}
	}
}

// WithBatchSize overrides the per-batch tenant count (default 50).
func WithBatchSize(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.batchSize = n
		}
	}
}

// WithWorkers overrides the worker-pool concurrency (default 8).
func WithWorkers(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.workers = n
		}
	}
}

// WithBackoffs overrides the exponential retry schedule. Use near-zero durations
// in tests to keep them fast. The number of entries IS the retry count; the
// default {1s,2s,4s} means ≤3 retries after the initial attempt (§7.7.4).
func WithBackoffs(ds []time.Duration) Option {
	return func(s *Scheduler) {
		if len(ds) > 0 {
			s.backoffs = append([]time.Duration(nil), ds...)
		}
	}
}

// WithIncrBuffer overrides the incremental-job channel capacity (default 256).
func WithIncrBuffer(n int) Option {
	return func(s *Scheduler) {
		if n > 0 {
			s.incrBuffer = n
		}
	}
}

// WithLocation overrides the cron schedule location (default time.Local).
func WithLocation(loc *time.Location) Option {
	return func(s *Scheduler) {
		if loc != nil {
			s.location = loc
		}
	}
}

// NewScheduler wires the pipeline + caches + tenant enumerator into a Scheduler.
// p/ch/repo should be non-nil; sem may be nil (L2 disabled, dev-friendly). The
// cron is constructed here but not started — call Start to schedule jobs. The
// incremental channel is buffered with incrBuffer.
func NewScheduler(p PipelineRunner, ch cache.Cache, sem semantic.SemanticCache, repo TenantEnumerator, opts ...Option) *Scheduler {
	s := &Scheduler{
		pipeline:   p,
		ch:         ch,
		sem:        sem,
		repo:       repo,
		log:        zap.NewNop(),
		now:        time.Now,
		batchSize:  defaultBatchSize,
		workers:    defaultWorkers,
		backoffs:   append([]time.Duration(nil), defaultBackoffs...),
		incrBuffer: defaultIncrBuffer,
		location:   time.Local,
	}
	for _, o := range opts {
		o(s)
	}
	s.incr = make(chan IncrementalJob, s.incrBuffer)
	s.cron = cron.New(cron.WithLocation(s.location))
	return s
}

// Start registers the batch + incremental cron jobs and starts the scheduler.
// It is idempotent: a second call is a no-op. Returns an error only if the cron
// rejects a (hard-coded, valid) spec — effectively never, but the error path is
// kept for contract fidelity (CONTRACT §13.3.5 Start() error).
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	if s.cron == nil {
		return fmt.Errorf("pregen: scheduler not initialised")
	}
	if _, err := s.cron.AddFunc(batchCronSpec, s.cronBatch); err != nil {
		return fmt.Errorf("pregen: add batch cron (%q): %w", batchCronSpec, err)
	}
	if _, err := s.cron.AddFunc(incrementalCronSpec, s.cronIncremental); err != nil {
		return fmt.Errorf("pregen: add incremental cron (%q): %w", incrementalCronSpec, err)
	}
	s.started = true
	s.cron.Start()
	s.log.Info("pregen scheduler started",
		zap.String("batch_spec", batchCronSpec),
		zap.String("incremental_spec", incrementalCronSpec))
	return nil
}

// cronBatch is the cron entry point for BatchGenerate: it wraps the ctx-accepting
// method with a fresh background context (cron jobs have no request ctx).
func (s *Scheduler) cronBatch() {
	n, err := s.BatchGenerate(context.Background())
	s.log.Info("pregen batch tick done", zap.Int("processed", n), zap.Error(err))
}

// cronIncremental is the cron entry point for IncrementalRetry.
func (s *Scheduler) cronIncremental() {
	n, err := s.IncrementalRetry(context.Background())
	s.log.Info("pregen incremental tick done", zap.Int("processed", n), zap.Error(err))
}

// Stop halts the cron and closes the incremental channel. It is idempotent.
// After Stop, EnqueueIncremental drops jobs with a warning (never panics on a
// closed channel) and IncrementalRetry drains remaining items then returns.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.incr != nil {
		close(s.incr)
	}
	s.mu.Unlock()
	if s.cron != nil {
		// cron.Stop returns a context that resolves when in-flight jobs finish;
		// we do not wait — §7.7 jobs are idempotent (re-run next tick).
		_ = s.cron.Stop()
	}
}

// EnqueueIncremental submits a daytime re-compute job. It never blocks: if the
// channel is full the job is dropped with a warning (§7.7.3 — production swaps
// the in-memory channel for Redis Stream with the same contract). After Stop it
// silently drops. The closed-flag check is under s.mu so we never send on a
// closed channel.
func (s *Scheduler) EnqueueIncremental(job IncrementalJob) {
	s.mu.Lock()
	if s.closed || s.incr == nil {
		s.mu.Unlock()
		s.log.Warn("pregen incremental drop: scheduler stopped",
			zap.Int64("tenant_id", job.TenantID), zap.String("reason", job.Reason))
		return
	}
	select {
	case s.incr <- job:
		s.mu.Unlock()
	default:
		s.mu.Unlock()
		s.log.Warn("pregen incremental drop: channel full",
			zap.Int64("tenant_id", job.TenantID), zap.String("reason", job.Reason))
	}
}
