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

// Inventory returns nearing-expiry / slow-moving / high-margin items. M3-S1
// best-effort: empty (no cross-product enumeration in M2 inventory); the
// pipeline's cold-start seed covers the empty case.
func (a signalLookuperAdapter) Inventory(_ context.Context, _, _ int64) ([]signal.InventorySig, error) {
	return nil, nil
}

// Members returns member-aggregate stats. M3-S1 best-effort: zero (membership
// has no List / birthday); cold-start covers.
func (a signalLookuperAdapter) Members(_ context.Context, _ int64) (signal.MemberStats, error) {
	return signal.MemberStats{}, nil
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
