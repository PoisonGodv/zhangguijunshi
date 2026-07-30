package pregen

import (
	"context"
	"fmt"
	"sync/atomic"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// BatchGenerate pre-generates today's advice for every active tenant (§7.7.2).
// Tenants are processed in batches of batchSize (default 50) with at most
// `workers` (default 8) concurrent pipeline runs. Each tenant's run retries up
// to len(backoffs) times on failure (default ≤3, exponential 1s/2s/4s). When a
// StoreEnumerator is wired, each tenant runs the pipeline PER STORE (StoreID
// injected into ctx); otherwise it falls back to a single storeID=0 run.
//
// Returns (processed, err): processed is the count of per-store runs whose
// Advice landed in both caches (with no StoreEnumerator this equals the tenant
// count, since each tenant is one storeID=0 run); err is non-nil if any run
// exhausted its retries — the batch still completes, because one tenant's/store's
// failure must NOT abort the rest (§7.7.4 "保证白天打开 App 有建议"). An empty
// ActiveTenantIDs result returns (0, nil) — no tenants is not an error
// (CONTRACT §13.5.8).
func (s *Scheduler) BatchGenerate(ctx context.Context) (int, error) {
	if s.repo == nil {
		return 0, fmt.Errorf("pregen: tenant enumerator not configured")
	}
	tenants, err := s.repo.ActiveTenantIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("pregen: enumerate tenants: %w", err)
	}
	if len(tenants) == 0 {
		s.log.Info("pregen batch: no active tenants")
		return 0, nil
	}

	var processed, failed int64
	for _, batch := range chunkInt64(tenants, s.batchSize) {
		// errgroup with a semaphore caps concurrency at s.workers. Goroutines
		// never return a non-nil error: a single tenant failure must not cancel
		// the in-flight batch (errgroup cancels its derived ctx on first error,
		// which we deliberately avoid). The derived ctx still propagates parent
		// cancellation so runWithRetry aborts promptly on shutdown.
		grp, gctx := errgroup.WithContext(ctx)
		grp.SetLimit(s.workers)
		for _, tid := range batch {
			tid := tid
			grp.Go(func() error {
				p, f := s.runTenantStores(gctx, tid, s.runWithRetry)
				atomic.AddInt64(&processed, int64(p))
				if f > 0 {
					atomic.AddInt64(&failed, int64(f))
					s.log.Warn("pregen tenant exhausted retries",
						zap.Int64("tenant_id", tid),
						zap.Int("failed_stores", f))
				}
				return nil
			})
		}
		_ = grp.Wait()
	}

	total := len(tenants)
	done := int(processed)
	s.log.Info("pregen batch complete",
		zap.Int("total", total),
		zap.Int("processed", done),
		zap.Int64("failed", failed))
	if failed > 0 {
		return done, fmt.Errorf("pregen: %d/%d tenants failed after retries", failed, total)
	}
	return done, nil
}

// chunkInt64 splits xs into contiguous runs of at most n. n<=0 returns xs as a
// single chunk. An empty/nil input yields an empty (non-nil) slice of chunks.
func chunkInt64(xs []int64, n int) [][]int64 {
	if n <= 0 {
		if len(xs) == 0 {
			return [][]int64{}
		}
		return [][]int64{xs}
	}
	out := make([][]int64, 0, (len(xs)+n-1)/n)
	for i := 0; i < len(xs); i += n {
		end := i + n
		if end > len(xs) {
			end = len(xs)
		}
		out = append(out, xs[i:end])
	}
	return out
}
