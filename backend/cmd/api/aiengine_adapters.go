package main

// aiengine_adapters.go wires the M3-S1 AI engine's anti-corruption ports to the
// concrete domain services (CONTRACT §12.3.2 signal.Lookuper, §12.3.9 exec's
// four ports). Each adapter is a thin struct that translates one domain call
// into the port's shape at the composition root, so aiengine imports no
// concrete domain package.
//
// M3-S1 best-effort signal: membership has no List method / Member has no
// Birthday field, and inventory's BatchRepo has no cross-product nearing list
// (ListActiveBatchesFIFO is per-product). The signalLookuperAdapter therefore
// returns empty / zero stats, and the pipeline's cold-start seed compensates
// (CONTRACT §12.5.5 + §12.8 显式延后). Real enumeration lands in M3-S2 alongside
// strategy_cache. Tests that need realistic signals inject a fake Lookuper
// directly into a hand-built Pipeline.

import (
	"context"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/aiengine/pregen"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/internal/reach"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// signalLookuperAdapter adapts catalog/inventory/membership/billing to
// signal.Lookuper. M3-S1 best-effort (see file comment).
type signalLookuperAdapter struct {
	catalogSvc    *catalog.Service
	inventorySvc  *inventory.Service
	membershipSvc *membership.Service
	billingSvc    *billing.Service
}

// Inventory returns nearing-expiry items by enumerating inventory batches
// (M3-S2: real enumeration via ListNearingBatches, replacing M3-S1's best-effort
// empty return). Each nearing batch is enriched with the product's name / cost /
// price via catalog.GetByID; a catalog miss leaves CostCents from the batch and
// PriceCents=0 (rule's margin guard then skips it). SlowMoving/HighMargin stay
// false (M3-S2 does not yet compute those tags here — dataquality integration
// is a later milestone).
func (a signalLookuperAdapter) Inventory(ctx context.Context, _, storeID int64) ([]signal.InventorySig, error) {
	if a.inventorySvc == nil {
		return nil, nil
	}
	batches, err := a.inventorySvc.ListNearingBatches(ctx, storeID, 3) // ≤3 天临期
	if err != nil {
		return nil, nil // best-effort: degrade to empty, cold-start covers
	}
	out := make([]signal.InventorySig, 0, len(batches))
	for _, b := range batches {
		if b == nil || b.Quantity <= 0 || b.ExpiryDate == nil {
			continue
		}
		days := int(time.Until(*b.ExpiryDate).Hours() / 24)
		sig := signal.InventorySig{
			ProductID:    b.ProductID,
			CostCents:    b.InPriceCents,
			Stock:        float64(b.Quantity),
			DaysToExpiry: days,
		}
		if a.catalogSvc != nil {
			if p, perr := a.catalogSvc.GetByID(ctx, b.ProductID); perr == nil && p != nil {
				sig.Name = p.Name
				sig.CostCents = p.CostCents // catalog's cost is authoritative
				sig.PriceCents = p.PriceCents
			}
		}
		out = append(out, sig)
	}
	return out, nil
}

// Members returns member-aggregate stats by enumerating members (M3-S2: real
// enumeration via ListMembers, replacing M3-S1's best-effort zero). Three
// scoped queries: sleep_30 (LastVisitAt < now-30d), birthday_today, high_value
// (Points ≥ 1000). Errors degrade to empty (cold-start covers).
func (a signalLookuperAdapter) Members(ctx context.Context, _ int64) (signal.MemberStats, error) {
	if a.membershipSvc == nil {
		return signal.MemberStats{}, nil
	}
	now := time.Now()
	sleepBefore := now.AddDate(0, 0, -30)
	sleepers, _ := a.membershipSvc.ListMembers(ctx, membership.MemberFilter{SleepBefore: &sleepBefore, Limit: 500})
	birthdays, _ := a.membershipSvc.ListMembers(ctx, membership.MemberFilter{BirthdayOn: &now, Limit: 500})
	highValue, _ := a.membershipSvc.ListMembers(ctx, membership.MemberFilter{MinPoints: 1000, Limit: 500})
	hvIDs := make([]int64, 0, len(highValue))
	for _, m := range highValue {
		hvIDs = append(hvIDs, m.ID)
	}
	bdIDs := make([]int64, 0, len(birthdays))
	for _, m := range birthdays {
		bdIDs = append(bdIDs, m.ID)
	}
	return signal.MemberStats{Sleep30: len(sleepers), HighValue: hvIDs, BirthdayToday: bdIDs}, nil
}

// DailyTxn returns the daily transaction aggregate. M3-S1 best-effort: zero
// (billing.DailySummary wiring deferred); cold-start covers.
func (a signalLookuperAdapter) DailyTxn(_ context.Context, _, _ int64, _ string) (signal.TxnSig, error) {
	return signal.TxnSig{}, nil
}

// couponIssuerAdapter adapts marketing.Service.Issue to exec.CouponIssuer.
// Source is fixed "ai_strategy"; strategyID is the attribution key.
type couponIssuerAdapter struct{ svc *marketing.Service }

func (a couponIssuerAdapter) Issue(ctx context.Context, memberID, couponID, strategyID int64, expireAt *time.Time) (*exec.CouponRef, error) {
	mc, err := a.svc.Issue(ctx, marketing.IssueInput{
		MemberID:   memberID,
		CouponID:   couponID,
		Source:     "ai_strategy",
		StrategyID: strategyID,
		ExpireAt:   expireAt,
	})
	if err != nil {
		return nil, err
	}
	return &exec.CouponRef{MemberCouponID: mc.ID, MemberID: mc.MemberID}, nil
}

// msgSenderAdapter adapts reach.Service.Send to exec.MessageSender. A blocked
// send returns (task, nil) at reach; this adapter copies task.Status /
// BlockedReason into SendRef so the executor attributes it (blocked is NOT
// failure).
type msgSenderAdapter struct{ svc *reach.Service }

func (a msgSenderAdapter) Send(ctx context.Context, storeID, memberID, strategyID int64, channel, tplCode string, vars map[string]string) (*exec.SendRef, error) {
	task, err := a.svc.Send(ctx, reach.SendInput{
		StoreID:      storeID,
		MemberID:     memberID,
		StrategyID:   strategyID,
		Channel:      channel,
		TemplateCode: tplCode,
		Vars:         vars,
	})
	if err != nil {
		return nil, err
	}
	return &exec.SendRef{TaskID: task.ID, Status: task.Status, BlockedReason: task.BlockedReason}, nil
}

// priceUpdaterAdapter adapts catalog.Service.UpdatePrice to exec.PriceUpdater.
type priceUpdaterAdapter struct{ svc *catalog.Service }

func (a priceUpdaterAdapter) UpdatePrice(ctx context.Context, productID, costCents, priceCents int64) error {
	_, err := a.svc.UpdatePrice(ctx, productID, costCents, priceCents)
	return err
}

// groupbuyOpenerAdapter adapts groupbuy.Service.Create to exec.GroupbuyOpener.
// strategyID is the attribution key written onto the Groupbuy row (M3-S3
// groupbuy→strategy linkage), so groupbuy_success can be attributed back.
type groupbuyOpenerAdapter struct{ svc *groupbuy.Service }

func (a groupbuyOpenerAdapter) Launch(ctx context.Context, storeID, productID int64, title string, priceCents int64, requiredCount, validHours int, strategyID int64) (*exec.GroupbuyRef, error) {
	g, err := a.svc.Create(ctx, groupbuy.CreateInput{
		StoreID:       storeID,
		ProductID:     productID,
		StrategyID:    strategyID,
		Title:         title,
		Params:        groupbuy.GroupbuyParams{PriceCents: priceCents},
		RequiredCount: requiredCount,
		ValidHours:    validHours,
	})
	if err != nil {
		return nil, err
	}
	return &exec.GroupbuyRef{GroupbuyID: g.ID}, nil
}

// tenantEnumeratorAdapter adapts the pregen scheduler's TenantEnumerator port.
// dev stub: ActiveTenantIDs returns nil (no real tenant store wired) so the
// pregen BatchGenerate is a no-op in dev; production wires tenant.Service.List
// (or a SQL enumerate) here to drive the 03:00 full-batch pre-generation.
type tenantEnumeratorAdapter struct{}

func (tenantEnumeratorAdapter) ActiveTenantIDs(_ context.Context) ([]int64, error) {
	return nil, nil
}

// strategyItemEnumeratorAdapter adapts strategy.Service onto pregen's
// StrategyItemEnumerator port (M3-S3 §14.7). ListExecutedSince walks the
// tenant's strategies and returns items in the executed state whose
// ExecutedAt is at or after `since`, each annotated with its strategy's
// StoreID (StrategyItem has no StoreID field). Cross-tenant records are never
// returned (strategy.Service resolves tenant from ctx via EnforceTenant).
// Limit caps the strategy scan (<=0 → 200) — a tuned pgx query replaces this
// once the pgx repo lands (CONTRACT §14.8).
type strategyItemEnumeratorAdapter struct{ svc *strategy.Service }

func (a strategyItemEnumeratorAdapter) ListExecutedSince(ctx context.Context, tenantID int64, since time.Time, limit int) ([]pregen.ExecutedItem, error) {
	if a.svc == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	strats, err := a.svc.List(withTenantCtx(ctx, tenantID), strategy.StrategyFilter{Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]pregen.ExecutedItem, 0, len(strats))
	for _, s := range strats {
		if s == nil {
			continue
		}
		for _, it := range s.Items {
			if it.ExecutionStatus != strategy.ItemExecuted || it.ExecutedAt == nil {
				continue
			}
			if it.ExecutedAt.Before(since) {
				continue
			}
			cp := it
			out = append(out, pregen.ExecutedItem{Item: &cp, StoreID: s.StoreID})
		}
	}
	return out, nil
}

// Compile-time: the adapters satisfy their respective anti-corruption ports.
var (
	_ attribution.CouponStats       = (*couponStatsAdapter)(nil)
	_ attribution.VisitCounter      = (*visitCounterAdapter)(nil)
	_ attribution.GroupbuyStats     = (*groupbuyStatsAdapter)(nil)
	_ attribution.RepurchaseCounter = (*repurchaseCounterAdapter)(nil)
	_ attribution.ItemLoader        = (*itemLoaderAdapter)(nil)
	_ pregen.StrategyItemEnumerator = (*strategyItemEnumeratorAdapter)(nil)
)

// withTenantCtx injects tenantID into ctx as a system-sourced identity so the
// anti-corruption port adapters can call domain services that resolve tenant
// via multitenancy.EnforceTenant(ctx). The M3-S3 attribution/bandit ports pass
// tenantID explicitly (CONTRACT §14.3 frozen signatures) AND the domain
// services re-derive it from ctx — we align the two here (CONTRACT §14.7).
func withTenantCtx(ctx context.Context, tenantID int64) context.Context {
	return multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		Source:   multitenancy.SourceSystem,
	})
}

// couponStatsAdapter adapts marketing.Service.ListByStrategy onto
// attribution.CouponStats. SentRedeemed returns the coupons issued
// (sent) and redeemed (used) under one strategy_id — the AI attribution link
// (marketing.MemberCoupon.StrategyID, M3-S3 additive). sent==0 means the
// strategy issued no coupons (non-coupon action) → the Computer skips the
// coupon_redeem metric. Errors degrade to (0,0,nil) (best-effort, §14.5).
type couponStatsAdapter struct{ svc *marketing.Service }

func (a couponStatsAdapter) SentRedeemed(ctx context.Context, tenantID, strategyID int64) (int, int, error) {
	if a.svc == nil || strategyID <= 0 {
		return 0, 0, nil
	}
	mcs, err := a.svc.ListByStrategy(withTenantCtx(ctx, tenantID), strategyID, "")
	if err != nil {
		return 0, 0, nil
	}
	sent := len(mcs)
	redeemed := 0
	for _, mc := range mcs {
		if mc != nil && mc.Status == marketing.MCStatusUsed {
			redeemed++
		}
	}
	return sent, redeemed, nil
}

// visitCounterAdapter adapts billing.Service.DailySummary onto
// attribution.VisitCounter. SinceCount sums paid order counts per natural day
// across [since, now]; BaselineCount returns the 7-day-pre average daily order
// count scaled to the same window duration (dev approximation, §14.7/§14.8 — a
// tuned pgx query replaces this once the pgx repo lands). Errors degrade to 0.
type visitCounterAdapter struct{ svc *billing.Service }

func (a visitCounterAdapter) SinceCount(ctx context.Context, tenantID, storeID int64, since time.Time) (int, error) {
	if a.svc == nil || storeID <= 0 {
		return 0, nil
	}
	bctx := withTenantCtx(ctx, tenantID)
	now := time.Now()
	total := 0
	for d := truncateDay(since); !d.After(now); d = d.AddDate(0, 0, 1) {
		s, err := a.svc.DailySummary(bctx, storeID, d)
		if err != nil {
			continue
		}
		total += s.OrderCount
	}
	return total, nil
}

func (a visitCounterAdapter) BaselineCount(ctx context.Context, tenantID, storeID int64, window time.Duration, before time.Time) (float64, error) {
	if a.svc == nil || storeID <= 0 || window <= 0 {
		return 0, nil
	}
	bctx := withTenantCtx(ctx, tenantID)
	// Average paid orders per day over the 7 days preceding `before`.
	var sum float64
	n := 0
	for i := 1; i <= 7; i++ {
		d := before.AddDate(0, 0, -i)
		s, err := a.svc.DailySummary(bctx, storeID, d)
		if err != nil {
			continue
		}
		sum += float64(s.OrderCount)
		n++
	}
	if n == 0 {
		return 0, nil
	}
	avgDaily := sum / float64(n)
	// Scale the per-day average to the actual window duration.
	return avgDaily * (float64(window) / float64(24*time.Hour)), nil
}

// groupbuyStatsAdapter adapts groupbuy onto attribution.GroupbuyStats.
// StartedSucceeded proxies to Service.StatsByStrategy so the groupbuy_success
// 归因 metric is computed from real groupbuy rows linked to the strategy_id
// (M3-S3 groupbuy→strategy linkage). strategyID<=0 or a nil svc yields
// (0,0,nil) → the Computer skips the metric (no panic, no leakage).
type groupbuyStatsAdapter struct{ svc *groupbuy.Service }

func (a groupbuyStatsAdapter) StartedSucceeded(ctx context.Context, tenantID, strategyID int64) (int, int, error) {
	if a.svc == nil || strategyID <= 0 {
		return 0, 0, nil
	}
	return a.svc.StatsByStrategy(withTenantCtx(ctx, tenantID), strategyID)
}

// repurchaseCounterAdapter adapts marketing.Service.MembersByStrategy +
// billing.Service.RepurchaseMembers onto attribution.RepurchaseCounter (the
// M3-S3 repurchase 归因 feed). Rate returns the share of the strategy-touched
// members who placed ≥1 paid order in [executedAt, measuredAt] (inclusive),
// clipped to [0,1]. A nil mkt/bill, an empty touched set, or any upstream error
// degrades to (0, nil) so the Computer's repurchase metric simply isn't
// emitted (best-effort, §14.5 — never panics, never leaks cross-tenant).
type repurchaseCounterAdapter struct {
	mkt  *marketing.Service
	bill *billing.Service
}

func (a repurchaseCounterAdapter) Rate(ctx context.Context, tenantID, strategyID int64, executedAt, measuredAt time.Time) (float64, error) {
	if a.mkt == nil || a.bill == nil || strategyID <= 0 {
		return 0, nil
	}
	mctx := withTenantCtx(ctx, tenantID)
	memberIDs, err := a.mkt.MembersByStrategy(mctx, tenantID, strategyID)
	if err != nil {
		return 0, nil
	}
	if len(memberIDs) == 0 {
		return 0, nil
	}
	n, err := a.bill.RepurchaseMembers(mctx, memberIDs, executedAt, measuredAt)
	if err != nil {
		return 0, nil
	}
	v := float64(n) / float64(len(memberIDs))
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v, nil
}

// itemLoaderAdapter adapts strategy.Service.GetItem onto attribution.ItemLoader
// (the Computer resolves an item to compute Reward without importing
// strategy.Service, CONTRACT §14.3.0 / §14.3.1).
type itemLoaderAdapter struct{ svc *strategy.Service }

func (a itemLoaderAdapter) GetItem(ctx context.Context, tenantID, itemID int64) (*strategy.StrategyItem, error) {
	if a.svc == nil {
		return nil, strategy.ErrNotFound
	}
	return a.svc.GetItem(withTenantCtx(ctx, tenantID), itemID)
}

// itemResolverAdapter adapts strategy.Service onto bandit.ItemResolver. Resolve
// maps an item to its arm key (ruleID = item.ReasonType), owning strategyID, and
// storeID (via the parent strategy). bandit never imports strategy (§14.3.0).
type itemResolverAdapter struct{ svc *strategy.Service }

func (a itemResolverAdapter) Resolve(ctx context.Context, tenantID, itemID int64) (string, int64, int64, error) {
	if a.svc == nil {
		return "", 0, 0, nil
	}
	bctx := withTenantCtx(ctx, tenantID)
	item, err := a.svc.GetItem(bctx, itemID)
	if err != nil {
		return "", 0, 0, err
	}
	storeID := int64(0)
	if strat, serr := a.svc.Get(bctx, item.StrategyID); serr == nil && strat != nil {
		storeID = strat.StoreID
	}
	return item.ReasonType, item.StrategyID, storeID, nil
}

// truncateDay returns t truncated to its natural day in the local location.
func truncateDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}
