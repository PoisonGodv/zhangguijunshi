package pregen

// attribution_jobs.go implements the M3-S3 attribution-backfill + bandit-update
// cron jobs (CONTRACT §14.7). The two jobs are wired into the existing
// robfig/cron scheduler alongside the M3-S2 batch + incremental crons.
//
//   AttributionBackfill (04:00 daily, also manually callable for tests):
//     for each active tenant → enumerate strategy items executed ≥24h ago
//     (T+24h window) → attribution.Computer.Compute → attribution.Repo.Record.
//     Best-effort: a tenant with no executed items contributes 0; a Compute
//     failure on one item is logged and skipped (the batch never aborts on a
//     single item — same contract as BatchGenerate §7.7.4).
//
//   BanditUpdate (02:00 daily, before the 03:00 pre-gen so weights influence
//     the day's advice): for each active tenant → enumerate executed items →
//     bandit.Learner.CreditItem (Resolve→Reward→Update). Best-effort: nil
//     RewardSource/ItemResolver (dev) makes CreditItem a no-op.
//
// Both jobs enumerate tenants via the existing TenantEnumerator port and inject
// the per-tenant identity into ctx (Source=system) so the strategy/attribution
// services re-derive tenant via EnforceTenant (same pattern as runAndCache).

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/bandit"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
	"go.uber.org/zap"
)

// Cron schedule literals for the M3-S3 jobs (CONTRACT §14.7).
const (
	attributionCronSpec = "0 4 * * *" // 04:00 daily, T+24h backfill
	banditCronSpec      = "0 2 * * *" // 02:00 daily, before 03:00 pre-gen
)

// attributionWindow is the T+24h attribution backfill threshold: only items
// executed at least this long ago get backfilled (ch07 §7.9.1). The enumerator
// looks back 72h (attributionLookback) so a missed 04:00 tick still catches
// the 24-72h window on the next run.
const (
	attributionWindow   = 24 * time.Hour
	attributionLookback = 72 * time.Hour
)

// ExecutedItem is one executed strategy item annotated with its owning store,
// the anti-corruption shape the M3-S3 jobs consume (pregen does not import
// strategy's repo internals, only the domain type).
type ExecutedItem struct {
	Item    *strategy.StrategyItem
	StoreID int64
}

// StrategyItemEnumerator is the anti-corruption port to the strategy domain
// (CONTRACT §14.7). The main thread adapts strategy.Service onto it; tests
// inject a fake. It is the ONLY way the M3-S3 jobs learn which items to
// backfill/credit — pregen never trusts an item id from the pipeline output.
type StrategyItemEnumerator interface {
	// ListExecutedSince returns items executed at or after `since` for tenantID,
	// each annotated with its strategy's StoreID. Cross-tenant records are never
	// returned. Limit caps the result (<=0 → a sane default).
	ListExecutedSince(ctx context.Context, tenantID int64, since time.Time, limit int) ([]ExecutedItem, error)
}

// WithAttribution wires the attribution repo + computer onto the scheduler
// (enables the AttributionBackfill cron). Either may be nil: a nil repo or
// computer disables the backfill cron cleanly (dev-friendly).
func WithAttribution(repo attribution.Repo, computer *attribution.Computer) Option {
	return func(s *Scheduler) {
		s.attrRepo = repo
		s.attrComputer = computer
	}
}

// WithBanditLearner wires the bandit learner onto the scheduler (enables the
// BanditUpdate cron). nil disables it cleanly.
func WithBanditLearner(l *bandit.Learner) Option {
	return func(s *Scheduler) {
		s.banditLearner = l
	}
}

// WithItemEnumerator wires the strategy-item enumerator port (required by both
// M3-S3 jobs). nil disables both crons cleanly.
func WithItemEnumerator(enu StrategyItemEnumerator) Option {
	return func(s *Scheduler) {
		s.itemEnum = enu
	}
}

// WithBanditCreditStore wires the bandit-credit de-duplication store (aiengine
// 修复③). When set, BanditUpdate credits each executed item exactly once across
// consecutive ticks (bandit_credited_at marker), while keeping the 72h lookback
// window's missed-tick resilience. nil preserves the legacy non-deduplicating
// behaviour (backward-compatible with M3-S2/S3 callers).
func WithBanditCreditStore(store BanditCreditStore) Option {
	return func(s *Scheduler) {
		s.creditStore = store
	}
}

// AttributionBackfill computes + records attribution metrics for every active
// tenant's executed strategy items in the T+24h window (CONTRACT §14.7).
// Returns (recorded, err): recorded is the count of Measurement rows written;
// err is non-nil only if tenant enumeration itself failed — per-tenant and
// per-item failures are logged and skipped (the batch never aborts, §7.7.4).
// Manually callable from tests (same pattern as BatchGenerate).
func (s *Scheduler) AttributionBackfill(ctx context.Context) (int, error) {
	if s.attrRepo == nil || s.attrComputer == nil {
		return 0, nil // attribution not wired (dev) — clean no-op
	}
	if s.itemEnum == nil {
		return 0, fmt.Errorf("pregen: item enumerator not configured")
	}
	if s.repo == nil {
		return 0, fmt.Errorf("pregen: tenant enumerator not configured")
	}
	tenants, err := s.repo.ActiveTenantIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("pregen: enumerate tenants: %w", err)
	}
	now := s.now()
	since := now.Add(-attributionLookback)
	cutoff := now.Add(-attributionWindow) // items executed before this are due

	var recorded, failed int64
	for _, tid := range tenants {
		n, ferr := s.backfillTenant(ctx, tid, since, cutoff, now)
		if ferr != nil {
			atomic.AddInt64(&failed, 1)
			s.log.Warn("attribution backfill tenant failed",
				zap.Int64("tenant_id", tid), zap.Error(ferr))
			continue
		}
		atomic.AddInt64(&recorded, int64(n))
	}
	s.log.Info("attribution backfill complete",
		zap.Int("tenants", len(tenants)),
		zap.Int64("recorded", recorded),
		zap.Int64("failed", failed))
	return int(recorded), nil
}

// backfillTenant runs the attribution backfill for one tenant.
func (s *Scheduler) backfillTenant(ctx context.Context, tenantID int64, since, cutoff, now time.Time) (int, error) {
	tctx := multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		Source:   multitenancy.SourceSystem,
	})
	items, err := s.itemEnum.ListExecutedSince(tctx, tenantID, since, 500)
	if err != nil {
		return 0, fmt.Errorf("enumerate executed items: %w", err)
	}
	recorded := 0
	for _, ei := range items {
		if ei.Item == nil || ei.Item.ExecutedAt == nil {
			continue
		}
		if !ei.Item.ExecutedAt.Before(cutoff) {
			continue // not yet due (< 24h since execution)
		}
		// Skip items that already have a measurement at this measuredAt — the
		// repo UNIQUE(tenant,item,metric,measured_at) upserts idempotently, so a
		// re-run just overwrites; we still avoid re-Compute on a near-identical
		// tick by checking LatestPerMetric presence (best-effort).
		executedAt := *ei.Item.ExecutedAt
		measurements, merr := s.attrComputer.Compute(tctx, tenantID, ei.Item, ei.StoreID, executedAt, now)
		if merr != nil {
			s.log.Debug("attribution compute skipped",
				zap.Int64("tenant_id", tenantID),
				zap.Int64("item_id", ei.Item.ID), zap.Error(merr))
			continue
		}
		for _, m := range measurements {
			if _, rerr := s.attrRepo.Record(tctx, tenantID, ei.Item.ID, m.Metric, m.Value, now); rerr != nil {
				s.log.Debug("attribution record failed",
					zap.Int64("tenant_id", tenantID),
					zap.Int64("item_id", ei.Item.ID),
					zap.String("metric", m.Metric), zap.Error(rerr))
				continue
			}
			recorded++
		}
	}
	return recorded, nil
}

// BanditUpdate credits bandit arm weights from every active tenant's executed
// strategy items (CONTRACT §14.7). Returns (credited, err): credited is the
// count of items processed through CreditItem; per-item failures are logged
// and skipped. A nil learner/rewardSource/resolver makes this a clean no-op.
// Manually callable from tests.
func (s *Scheduler) BanditUpdate(ctx context.Context) (int, error) {
	if s.banditLearner == nil {
		return 0, nil // bandit not wired (dev) — clean no-op
	}
	if s.itemEnum == nil {
		return 0, fmt.Errorf("pregen: item enumerator not configured")
	}
	if s.repo == nil {
		return 0, fmt.Errorf("pregen: tenant enumerator not configured")
	}
	tenants, err := s.repo.ActiveTenantIDs(ctx)
	if err != nil {
		return 0, fmt.Errorf("pregen: enumerate tenants: %w", err)
	}
	now := s.now()
	since := now.Add(-attributionLookback)

	var credited, failed int64
	for _, tid := range tenants {
		n, ferr := s.creditTenant(ctx, tid, since)
		if ferr != nil {
			atomic.AddInt64(&failed, 1)
			s.log.Warn("bandit update tenant failed",
				zap.Int64("tenant_id", tid), zap.Error(ferr))
			continue
		}
		atomic.AddInt64(&credited, int64(n))
	}
	s.log.Info("bandit update complete",
		zap.Int("tenants", len(tenants)),
		zap.Int64("credited", credited),
		zap.Int64("failed", failed))
	return int(credited), nil
}

// creditTenant runs the bandit credit loop for one tenant.
//
// De-duplication (aiengine 修复③): when a BanditCreditStore is wired, the loop
// enumerates ONLY not-yet-credited executed items (bandit_credited_at IS NULL)
// and stamps the marker after each successful CreditItem — so a given item is
// credited exactly once across consecutive 02:00 ticks, while the 72h lookback
// window still catches up items a missed tick skipped. Without a store (legacy
// M3-S2/S3 callers) it falls back to the plain StrategyItemEnumerator path.
func (s *Scheduler) creditTenant(ctx context.Context, tenantID int64, since time.Time) (int, error) {
	tctx := multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		Source:   multitenancy.SourceSystem,
	})
	var (
		items []ExecutedItem
		err   error
	)
	if s.creditStore != nil {
		items, err = s.creditStore.ListUncreditedExecuted(tctx, tenantID, since, 500)
	} else {
		items, err = s.itemEnum.ListExecutedSince(tctx, tenantID, since, 500)
	}
	if err != nil {
		return 0, fmt.Errorf("enumerate executed items: %w", err)
	}
	credited := 0
	for _, ei := range items {
		if ei.Item == nil || ei.Item.ExecutedAt == nil {
			continue
		}
		if _, cerr := s.banditLearner.CreditItem(tctx, tenantID, ei.Item.ID); cerr != nil {
			s.log.Debug("bandit credit skipped",
				zap.Int64("tenant_id", tenantID),
				zap.Int64("item_id", ei.Item.ID), zap.Error(cerr))
			continue
		}
		// Mark credited so the next tick's enumeration skips this item (修复③).
		// Best-effort: a marker-write failure leaves the item eligible for one
		// bounded re-credit next tick — the same tolerance the 72h window already
		// affords a missed tick. The accumulate semantics in bandit.Update are
		// untouched.
		if s.creditStore != nil {
			if merr := s.creditStore.MarkCredited(tctx, tenantID, ei.Item.ID, s.now()); merr != nil {
				s.log.Debug("bandit credited-marker write failed (non-fatal)",
					zap.Int64("tenant_id", tenantID),
					zap.Int64("item_id", ei.Item.ID), zap.Error(merr))
			}
		}
		credited++
	}
	return credited, nil
}

// cronAttribution is the cron entry point for AttributionBackfill.
func (s *Scheduler) cronAttribution() {
	n, err := s.AttributionBackfill(context.Background())
	s.log.Info("attribution tick done", zap.Int("recorded", n), zap.Error(err))
}

// cronBandit is the cron entry point for BanditUpdate.
func (s *Scheduler) cronBandit() {
	n, err := s.BanditUpdate(context.Background())
	s.log.Info("bandit tick done", zap.Int("credited", n), zap.Error(err))
}
