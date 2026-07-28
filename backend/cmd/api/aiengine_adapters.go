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

	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/internal/reach"
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
// M2-S2 groupbuy rows carry no strategy_id; only the GroupbuyID is returned.
type groupbuyOpenerAdapter struct{ svc *groupbuy.Service }

func (a groupbuyOpenerAdapter) Launch(ctx context.Context, storeID, productID int64, title string, priceCents int64, requiredCount, validHours int, _ int64) (*exec.GroupbuyRef, error) {
	g, err := a.svc.Create(ctx, groupbuy.CreateInput{
		StoreID:       storeID,
		ProductID:     productID,
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
