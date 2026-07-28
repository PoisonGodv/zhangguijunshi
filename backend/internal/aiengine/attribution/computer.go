package attribution

import (
	"context"
	"math"
	"time"

	"github.com/zhangguijunshi/backend/internal/strategy"
)

// CouponStats is the anti-corruption port → marketing. It returns the sent /
// redeemed coupon counts under one strategy. The main thread adapts the
// marketing service onto it; nil means "no coupon linkage" and is skipped.
type CouponStats interface {
	SentRedeemed(ctx context.Context, tenantID, strategyID int64) (sent, redeemed int, err error)
}

// VisitCounter is the anti-corruption port → billing. SinceCount is the
// store's payment count since t; BaselineCount is the same-duration historical
// average ending at `before`. The main thread adapts the billing service.
type VisitCounter interface {
	SinceCount(ctx context.Context, tenantID, storeID int64, since time.Time) (int, error)
	BaselineCount(ctx context.Context, tenantID, storeID int64, window time.Duration, before time.Time) (float64, error)
}

// GroupbuyStats is the anti-corruption port → groupbuy. It returns the
// started / succeeded groupbuy counts linked to one strategy. No linkage
// returns (0, 0, nil) → groupbuy_success is not emitted.
type GroupbuyStats interface {
	StartedSucceeded(ctx context.Context, tenantID, strategyID int64) (started, succeeded int, err error)
}

// RepurchaseCounter is the anti-corruption port → marketing+billing. Rate
// returns the repurchase rate of the members a strategy touched over
// [executedAt, measuredAt]: distinct members with ≥1 paid order in the window
// divided by the touched-member count (clipped to [0,1] by the caller). The
// main thread adapts marketing.Service.MembersByStrategy + billing.Service.
// RepurchaseMembers onto it; nil means "no repurchase linkage" and is skipped.
type RepurchaseCounter interface {
	Rate(ctx context.Context, tenantID, strategyID int64, executedAt, measuredAt time.Time) (float64, error)
}

// Sources bundles the anti-corruption ports. Any may be nil (dev wires none);
// Compute skips the corresponding metric rather than panicking.
type Sources struct {
	Coupons    CouponStats
	Visits     VisitCounter
	Groupbuy   GroupbuyStats
	Repurchase RepurchaseCounter
}

// ItemLoader is the anti-corruption port → strategy.Service: it resolves an
// item by (tenantID, itemID) for Reward, which has no item parameter. Computer
// does NOT import strategy.Service (no import cycle); the main thread adapts
// strategy.Service onto this narrow interface. Cross-tenant / missing items
// yield strategy.ErrNotFound, which Reward propagates.
type ItemLoader interface {
	GetItem(ctx context.Context, tenantID, itemID int64) (*strategy.StrategyItem, error)
}

// nowFunc is the injected clock; tests pin it, production defaults to time.Now.
type nowFunc func() time.Time

// Option configures a Computer (functional-option idiom).
type Option func(*Computer)

// WithNowFunc injects a clock so Reward's measuredAt is deterministic under
// test (production leaves the default time.Now).
func WithNowFunc(f func() time.Time) Option {
	return func(c *Computer) {
		if f != nil {
			c.now = f
		}
	}
}

// Computer derives attribution Measurements for an executed strategy_item and
// folds them into a normalised [0,1] reward for the bandit. It owns metric
// semantics: coupon_redeem / visit_uplift / groupbuy_success are computed this
// sprint; repurchase / moments_engage have no data source and are not emitted.
// Ports are queried selectively by item.ActionType; nil ports are skipped.
type Computer struct {
	src    Sources
	loader ItemLoader
	now    nowFunc
}

// NewComputer constructs a Computer. loader is required (the main thread adapts
// strategy.Service onto ItemLoader); opts override the clock.
func NewComputer(src Sources, loader ItemLoader, opts ...Option) *Computer {
	c := &Computer{
		src:    src,
		loader: loader,
		now:    time.Now,
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	return c
}

// couponActions are the action_types whose effect is measured by coupon
// redemption (CONTRACT §14.3.1 A1). String literals because attribution
// imports strategy, not rule (DAG §14.3.0).
var couponActions = map[string]bool{
	"coupon_wake":    true,
	"recharge_promo": true,
}

// groupbuyActions are the action_types measured by groupbuy success.
var groupbuyActions = map[string]bool{
	"groupbuy": true,
}

// Compute evaluates item at measuredAt and returns the computable metric
// facts (empty slice, nil error when nothing is computable). tenantID is the
// explicit multi-tenant key: when it disagrees with item.TenantID the item is
// foreign and Compute returns an empty slice without querying any port (no
// cross-tenant leakage). item is read-only.
//
//	coupon_redeem    = redeemed/sent        (coupon_wake|recharge_promo; sent==0 → skip)
//	visit_uplift     = SinceCount(executedAt) − BaselineCount(window=measuredAt−executedAt, before=executedAt)
//	groupbuy_success = succeeded/started    (groupbuy; started==0 → skip)
//
// A nil port skips its metric (no panic). A nil item yields ErrParam.
func (c *Computer) Compute(ctx context.Context, tenantID int64, item *strategy.StrategyItem, storeID int64, executedAt, measuredAt time.Time) ([]Measurement, error) {
	if item == nil {
		return nil, ErrParam
	}
	// Cross-tenant: the item belongs to another tenant — treat as absent, emit
	// nothing, query nothing (no leakage through the marketing/billing ports).
	if item.TenantID != tenantID {
		return nil, nil
	}
	out := make([]Measurement, 0)
	strategyID := item.StrategyID

	// coupon_redeem — only coupon-channel items, only when a port is wired.
	if couponActions[item.ActionType] && c.src.Coupons != nil {
		if sent, redeemed, err := c.src.Coupons.SentRedeemed(ctx, tenantID, strategyID); err == nil && sent > 0 {
			out = append(out, Measurement{
				TenantID:       tenantID,
				StrategyItemID: item.ID,
				Metric:         MetricCouponRedeem,
				Value:          float64(redeemed) / float64(sent),
				MeasuredAt:     measuredAt,
			})
		}
	}

	// visit_uplift — any item, needs a positive measurement window.
	if c.src.Visits != nil {
		window := measuredAt.Sub(executedAt)
		if window > 0 {
			baseline, bErr := c.src.Visits.BaselineCount(ctx, tenantID, storeID, window, executedAt)
			since, sErr := c.src.Visits.SinceCount(ctx, tenantID, storeID, executedAt)
			if bErr == nil && sErr == nil {
				out = append(out, Measurement{
					TenantID:       tenantID,
					StrategyItemID: item.ID,
					Metric:         MetricVisitUplift,
					Value:          float64(since) - baseline,
					MeasuredAt:     measuredAt,
				})
			}
		}
	}

	// groupbuy_success — only groupbuy items, only when a port is wired.
	if groupbuyActions[item.ActionType] && c.src.Groupbuy != nil {
		if started, succeeded, err := c.src.Groupbuy.StartedSucceeded(ctx, tenantID, strategyID); err == nil && started > 0 {
			out = append(out, Measurement{
				TenantID:       tenantID,
				StrategyItemID: item.ID,
				Metric:         MetricGroupbuySuccess,
				Value:          float64(succeeded) / float64(started),
				MeasuredAt:     measuredAt,
			})
		}
	}

	// repurchase — coupon-channel items (the strategy touched members via coupon
	// issuance), only when the repurchase port is wired. The window is
	// [executedAt, measuredAt]; errors degrade to "no metric" (no panic).
	if couponActions[item.ActionType] && c.src.Repurchase != nil {
		if rate, rerr := c.src.Repurchase.Rate(ctx, tenantID, strategyID, executedAt, measuredAt); rerr == nil {
			out = append(out, Measurement{
				TenantID:       tenantID,
				StrategyItemID: item.ID,
				Metric:         MetricRepurchase,
				Value:          clip01(rate),
				MeasuredAt:     measuredAt,
			})
		}
	}

	return out, nil
}

// Reward returns the item's normalised [0,1] reward for the bandit. It
// resolves the item via the injected ItemLoader (cross-tenant → the loader's
// ErrNotFound propagates), then folds the same coupon/visit signals Compute
// uses into:
//
//	reward = 0.7*coupon_rate + 0.3*visit_delta_norm,  clip [0,1]
//
// coupon_rate = redeemed/sent (0 when the item has no coupon channel, no port,
// or sent==0). visit_delta_norm = visit_uplift normalised to [0,1] by dividing
// by max(1, baseline) then clipping. executedAt = item.ExecutedAt;
// measuredAt = the injected clock (WithNowFunc for deterministic tests). A nil
// port or missing executedAt zeroes its term rather than erroring. Satisfies
// bandit.RewardSource.
func (c *Computer) Reward(ctx context.Context, tenantID, itemID, strategyID, storeID int64) (float64, error) {
	item, err := c.loader.GetItem(ctx, tenantID, itemID)
	if err != nil {
		return 0, err
	}
	if item == nil {
		return 0, ErrNotFound
	}

	couponRate := 0.0
	if couponActions[item.ActionType] && c.src.Coupons != nil {
		if sent, redeemed, err := c.src.Coupons.SentRedeemed(ctx, tenantID, strategyID); err == nil && sent > 0 {
			couponRate = float64(redeemed) / float64(sent)
		}
	}

	visitNorm := 0.0
	if c.src.Visits != nil && item.ExecutedAt != nil {
		executedAt := *item.ExecutedAt
		measuredAt := c.now()
		if window := measuredAt.Sub(executedAt); window > 0 {
			if baseline, bErr := c.src.Visits.BaselineCount(ctx, tenantID, storeID, window, executedAt); bErr == nil {
				if since, sErr := c.src.Visits.SinceCount(ctx, tenantID, storeID, executedAt); sErr == nil {
					delta := float64(since) - baseline
					visitNorm = delta / math.Max(1, baseline)
				}
			}
		}
	}

	reward := 0.7*couponRate + 0.3*visitNorm
	return clip01(reward), nil
}

// clip01 clamps v into [0,1] (CONTRACT §14.5.8 reward clip).
func clip01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}

// Compile-time guard: Computer satisfies the bandit.RewardSource contract (the
// frozen Reward signature) — kept local to avoid importing bandit (DAG).
var _ interface {
	Reward(ctx context.Context, tenantID, itemID, strategyID, storeID int64) (float64, error)
} = (*Computer)(nil)
