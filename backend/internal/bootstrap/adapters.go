package bootstrap

// adapters.go hosts the anti-corruption adapters the composition root uses to
// bind concrete domain services onto the narrower ports of neighbouring
// domains (CONTRACT §8.3 / §9.3 / §10.3 / §11.3.2 / §12.3 / §14.3 / §15.7).
// These were previously spread across cmd/api/{main,aiengine_adapters,
// payment_adapters}.go; they moved here so the api / worker / cron binaries all
// compose the identical object graph. Logic is unchanged.

import (
	"context"
	"errors"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/aiengine/pregen"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/compliance/contentfilter"
	"github.com/zhangguijunshi/backend/internal/compliance/freqcircuit"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/internal/payment"
	"github.com/zhangguijunshi/backend/internal/reach"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/internal/subscription"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// defaultMarginFloorPct is the gross-margin floor (10%) the marketing margin
// guard enforces on coupon issuance.
const defaultMarginFloorPct = 10

// ---- catalog ↔ billing / inventory / marketing / margin ----

// catalogPricer adapts *catalog.Service to billing.ProductPricer (CONTRACT §8.3).
type catalogPricer struct{ svc *catalog.Service }

func (p catalogPricer) GetByID(ctx context.Context, tenantID, id int64) (*billing.PricedProduct, error) {
	prod, err := p.svc.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if prod.TenantID != tenantID {
		return nil, catalog.ErrNotFound
	}
	return &billing.PricedProduct{
		ID:         prod.ID,
		TenantID:   prod.TenantID,
		StoreID:    prod.StoreID,
		Name:       prod.Name,
		Unit:       prod.Unit,
		PriceCents: prod.PriceCents,
		CostCents:  prod.CostCents,
		Status:     prod.Status,
	}, nil
}

// catalogProductLookuper adapts *catalog.Service to inventory.ProductLookuper
// (CONTRACT §9.3).
type catalogProductLookuper struct{ svc *catalog.Service }

func (l catalogProductLookuper) Exists(ctx context.Context, tenantID, productID int64) (bool, error) {
	prod, err := l.svc.GetByID(ctx, productID)
	if err != nil {
		if errors.Is(err, catalog.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if prod.TenantID != tenantID {
		return false, nil
	}
	return true, nil
}

// inventoryStockConsumer adapts *inventory.Service to billing.StockConsumer
// (CONTRACT §9.3).
type inventoryStockConsumer struct{ svc *inventory.Service }

func (c inventoryStockConsumer) ConsumeFIFO(ctx context.Context, storeID, productID, qty, refOrderID int64) error {
	_, err := c.svc.ConsumeFIFO(ctx, storeID, productID, qty, refOrderID)
	return err
}

// membershipLookuper adapts *membership.Service to marketing.MemberLookuper
// (CONTRACT §10.3).
type membershipLookuper struct{ svc *membership.Service }

func (l membershipLookuper) Exists(ctx context.Context, tenantID, memberID int64) (bool, error) {
	m, err := l.svc.GetMember(ctx, memberID)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if m.TenantID != tenantID {
		return false, nil
	}
	return true, nil
}

// catalogMarginGuard adapts margin.Guard + *catalog.Service to
// marketing.MarginGuard (CONTRACT §10.3). Best-effort: a catalog read failure
// skips the check rather than blocking issuance.
type catalogMarginGuard struct {
	guard      margin.Guard
	catalogSvc *catalog.Service
}

func (g catalogMarginGuard) ValidateCoupon(ctx context.Context, c *marketing.Coupon) error {
	prods, err := g.catalogSvc.List(ctx, catalog.ProductFilter{Limit: 500})
	if err != nil {
		return nil
	}
	products := make([]margin.ProductView, 0, len(prods))
	for _, p := range prods {
		products = append(products, margin.ProductView{
			ID:         p.ID,
			CostCents:  p.CostCents,
			PriceCents: p.PriceCents,
		})
	}
	rule := margin.CouponRule{
		Type:             c.Type,
		ThresholdCents:   c.Params.ThresholdCents,
		OffCents:         c.Params.OffCents,
		DiscountPermille: c.Params.DiscountPermille,
	}
	return margin.ValidateCoupon(g.guard, rule, products)
}

// ---- reach ↔ freqcircuit / contentfilter ----

// freqGateAdapter adapts *freqcircuit.Service to reach.FreqGate (CONTRACT §11.3.2).
type freqGateAdapter struct{ svc *freqcircuit.Service }

func (a freqGateAdapter) Allow(ctx context.Context, channel string) (bool, string, error) {
	return a.svc.Allow(ctx, freqcircuit.Channel(channel))
}

// cfFilterAdapter adapts *contentfilter.Filter to reach.ContentFilter
// (CONTRACT §11.3.2).
type cfFilterAdapter struct{ f *contentfilter.Filter }

func (a cfFilterAdapter) Do(text string) reach.ContentResult {
	r := a.f.Do(text)
	return reach.ContentResult{
		Text:     r.Text,
		Passed:   r.Passed,
		HitWords: r.HitWords,
		Replaced: r.Replaced,
	}
}

// ---- payment ↔ billing / membership / subscription ----

// paymentFulfiller routes a successful payment to the domain its Subject names
// (CONTRACT §15.7). nil domains are skipped.
type paymentFulfiller struct {
	bill *billing.Service
	mbr  *membership.Service
	sub  *subscription.Service
}

var _ payment.Fulfiller = (*paymentFulfiller)(nil)

func (f paymentFulfiller) Fulfill(ctx context.Context, p *payment.Payment) error {
	switch p.Subject {
	case payment.SubjectOrder:
		if f.bill == nil || p.PaidAt == nil {
			return nil
		}
		_, err := f.bill.MarkOrderPaid(ctx, p.RefID, p.OutTradeNo, *p.PaidAt)
		return err
	case payment.SubjectRecharge:
		if f.mbr == nil {
			return nil
		}
		_, err := f.mbr.Recharge(ctx, membership.RechargeInput{
			MemberID:     p.RefID,
			AmountCents:  p.AmountCents,
			RefPaymentID: p.ID,
		})
		return err
	case payment.SubjectSubscription:
		if f.sub == nil {
			return nil
		}
		_, err := f.sub.ConfirmPayment(ctx, p.TenantID, p.AmountCents, p.Provider, p.OutTradeNo)
		return err
	}
	return nil
}

// ---- aiengine ↔ domain services (M3-S1 §12.3 / M3-S3 §14.3) ----

// withTenantCtx injects tenantID as a system-sourced identity so the adapters
// can call domain services that resolve tenant via multitenancy.EnforceTenant.
func withTenantCtx(ctx context.Context, tenantID int64) context.Context {
	return multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		Source:   multitenancy.SourceSystem,
	})
}

// signalLookuperAdapter adapts catalog/inventory/membership/billing to
// signal.Lookuper.
type signalLookuperAdapter struct {
	catalogSvc    *catalog.Service
	inventorySvc  *inventory.Service
	membershipSvc *membership.Service
	billingSvc    *billing.Service
}

func (a signalLookuperAdapter) Inventory(ctx context.Context, _, storeID int64) ([]signal.InventorySig, error) {
	if a.inventorySvc == nil {
		return nil, nil
	}
	batches, err := a.inventorySvc.ListNearingBatches(ctx, storeID, 3)
	if err != nil {
		return nil, nil
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
				sig.CostCents = p.CostCents
				sig.PriceCents = p.PriceCents
			}
		}
		out = append(out, sig)
	}
	return out, nil
}

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

func (a signalLookuperAdapter) DailyTxn(_ context.Context, _, _ int64, _ string) (signal.TxnSig, error) {
	return signal.TxnSig{}, nil
}

// couponIssuerAdapter adapts marketing.Service.Issue to exec.CouponIssuer.
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

// msgSenderAdapter adapts reach.Service.Send to exec.MessageSender.
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
// dev stub: ActiveTenantIDs returns nil so the pregen BatchGenerate is a no-op
// until production wires a real tenant store.
type tenantEnumeratorAdapter struct{}

func (tenantEnumeratorAdapter) ActiveTenantIDs(_ context.Context) ([]int64, error) {
	return nil, nil
}

// strategyItemEnumeratorAdapter adapts strategy.Service onto pregen's
// StrategyItemEnumerator port (M3-S3 §14.7).
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

var (
	_ attribution.CouponStats       = (*couponStatsAdapter)(nil)
	_ attribution.VisitCounter      = (*visitCounterAdapter)(nil)
	_ attribution.GroupbuyStats     = (*groupbuyStatsAdapter)(nil)
	_ attribution.RepurchaseCounter = (*repurchaseCounterAdapter)(nil)
	_ attribution.ItemLoader        = (*itemLoaderAdapter)(nil)
	_ pregen.StrategyItemEnumerator = (*strategyItemEnumeratorAdapter)(nil)
)

// couponStatsAdapter adapts marketing.Service.ListByStrategy onto
// attribution.CouponStats.
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
// attribution.VisitCounter.
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
	return avgDaily * (float64(window) / float64(24*time.Hour)), nil
}

// groupbuyStatsAdapter adapts groupbuy onto attribution.GroupbuyStats.
type groupbuyStatsAdapter struct{ svc *groupbuy.Service }

func (a groupbuyStatsAdapter) StartedSucceeded(ctx context.Context, tenantID, strategyID int64) (int, int, error) {
	if a.svc == nil || strategyID <= 0 {
		return 0, 0, nil
	}
	return a.svc.StatsByStrategy(withTenantCtx(ctx, tenantID), strategyID)
}

// repurchaseCounterAdapter adapts marketing.MembersByStrategy +
// billing.RepurchaseMembers onto attribution.RepurchaseCounter.
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

// itemLoaderAdapter adapts strategy.Service.GetItem onto attribution.ItemLoader.
type itemLoaderAdapter struct{ svc *strategy.Service }

func (a itemLoaderAdapter) GetItem(ctx context.Context, tenantID, itemID int64) (*strategy.StrategyItem, error) {
	if a.svc == nil {
		return nil, strategy.ErrNotFound
	}
	return a.svc.GetItem(withTenantCtx(ctx, tenantID), itemID)
}

// itemResolverAdapter adapts strategy.Service onto bandit.ItemResolver.
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
