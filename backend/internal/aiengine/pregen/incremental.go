package pregen

import (
	"context"

	"go.uber.org/zap"
)

// IncrementalRetry drains the incremental-job channel (non-blocking) and
// re-runs the pipeline for each pending tenant, overwriting both caches
// (§7.7.3). Each job is attempted exactly once per tick — IncrementalRetry does
// NOT do within-tick exponential retry (unlike BatchGenerate); a failed job is
// logged and dropped, and the next cron tick (or a fresh EnqueueIncremental)
// handles newly-enqueued work. The "Retry" in the name refers to the cron-level
// cadence (every 10 min), not within-tick retry.
//
// Returns (processed, err): processed is the count of jobs that landed in both
// caches this tick; err is always nil (a single job failure is logged, not
// surfaced as a batch error — consistent with "incremental best-effort").
func (s *Scheduler) IncrementalRetry(ctx context.Context) (int, error) {
	processed := 0
	for {
		// Non-blocking drain: exit as soon as the channel has no buffered job.
		// A closed channel yields (zero, false) immediately — handled by ok.
		select {
		case job, ok := <-s.incr:
			if !ok {
				// Channel closed by Stop: stop draining.
				return processed, nil
			}
			if err := s.runAndCache(ctx, job.TenantID); err != nil {
				s.log.Warn("pregen incremental job failed",
					zap.Int64("tenant_id", job.TenantID),
					zap.String("reason", job.Reason),
					zap.Error(err))
				continue
			}
			processed++
		default:
			return processed, nil
		}
	}
}
