package exec

// executor.go implements the Executor (BE-B-11 ActionExecutor, CONTRACT
// §12.3.9). Run is the handler's single call surface: load item → dispatch on
// ActionType → drive the ports → persist the item's terminal execution_status
// → attribute the outcome. Handlers return either a fully-formed *ExecResult
// (executed / port-driven blocked-or-failed) or an intrinsic-block sentinel in
// the error channel; Run unifies both into the final ExecResult + item state.

import (
	"context"
	"errors"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Executor is the one-click ActionExecutor. It owns no business state; the
// strategy service (item + state machine), the shared margin guard (third
// defense-in-depth check, identical config to rule / agent) and the four
// anti-corruption ports are injected. Any port may be nil — the corresponding
// handler then returns blocked(not_configured) instead of nil-dereferencing.
type Executor struct {
	coupons  CouponIssuer
	reacher  MessageSender
	pricer   PriceUpdater
	groupbuy GroupbuyOpener
	guard    margin.Guard
	svc      *strategy.Service
	now      func() time.Time
}

// Option customises an Executor.
type Option func(*Executor)

// WithNowFunc overrides the executor clock (deterministic coupon expire-at
// tests). A nil fallback is ignored.
func WithNowFunc(f func() time.Time) Option {
	return func(e *Executor) {
		if f != nil {
			e.now = f
		}
	}
}

// NewExecutor wires the strategy service, the shared margin guard and the four
// anti-corruption ports (CONTRACT §12.3.9 NewExecutor). Any port may be nil.
func NewExecutor(svc *strategy.Service, guard margin.Guard, coupons CouponIssuer, reacher MessageSender, pricer PriceUpdater, groupbuy GroupbuyOpener, opts ...Option) *Executor {
	e := &Executor{
		coupons:  coupons,
		reacher:  reacher,
		pricer:   pricer,
		groupbuy: groupbuy,
		guard:    guard,
		svc:      svc,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// ExecuteInput is the boss's one-click request.
type ExecuteInput struct {
	StrategyItemID int64
	Confirm        bool // price_adj requires explicit boss confirmation
}

// Run loads the item from the repo (never trusting caller-supplied detail),
// dispatches on ActionType, drives the ports, persists the item's terminal
// execution_status, and returns the attributed outcome.
//
// First line EnforceTenant (CONTRACT §12.5.1). A cross-tenant / absent item
// surfaces as strategy.ErrNotFound (the caller maps it to HTTP 404); this is
// returned BEFORE any state mutation. Intrinsic blocks (sentinels) →
// ExecResult{blocked} + ItemBlocked, nil error. Port-driven outcomes
// (executed / send-blocked / send-failed) → ExecResult carries the Status, and
// the item state machine follows it. A real system error (non-sentinel) →
// ItemFailed + the error returned to the caller.
func (e *Executor) Run(ctx context.Context, in ExecuteInput) (*ExecResult, error) {
	if _, err := multitenancy.EnforceTenant(ctx); err != nil {
		return nil, err
	}
	if in.StrategyItemID <= 0 {
		return nil, strategy.ErrParam
	}
	if e.svc == nil {
		return nil, ErrPortNotConfigured
	}

	item, err := e.svc.GetItem(ctx, in.StrategyItemID)
	if err != nil {
		// ErrNotFound (cross-tenant / absent) or ErrParam — surface untouched.
		return nil, err
	}

	res, err := e.dispatch(ctx, item, in)
	if err != nil {
		// Intrinsic block sentinel → ExecResult{blocked} + ItemBlocked (nil err).
		if reason, ok := blockedReason(err); ok {
			if _, ferr := e.svc.UpdateItemStatus(ctx, item.ID, strategy.ItemBlocked); ferr != nil {
				// State-machine write failed; the blocked outcome still stands.
			}
			return &ExecResult{StrategyItemID: item.ID, Status: strategy.ItemBlocked, Reason: reason}, nil
		}
		// Real system fault → ItemFailed + return the error.
		_, _ = e.svc.UpdateItemStatus(ctx, item.ID, strategy.ItemFailed)
		return nil, err
	}

	// Port-driven outcome: map the handler-reported Status to the item state.
	itemStatus := strategy.ItemFailed
	switch res.Status {
	case strategy.ItemExecuted:
		itemStatus = strategy.ItemExecuted
	case strategy.ItemBlocked:
		itemStatus = strategy.ItemBlocked
	case strategy.ItemFailed:
		itemStatus = strategy.ItemFailed
	}
	_, _ = e.svc.UpdateItemStatus(ctx, item.ID, itemStatus)
	res.StrategyItemID = item.ID
	return res, nil
}

// dispatch routes to one of 6 handlers by ActionType ONLY (CONTRACT §12.5.9).
// ReasonType is consulted solely inside coupon_wake's template selection.
func (e *Executor) dispatch(ctx context.Context, item *strategy.StrategyItem, in ExecuteInput) (*ExecResult, error) {
	switch item.ActionType {
	case rule.ActGroupSeckill:
		return e.execGroupSeckill(ctx, item)
	case rule.ActCouponWake:
		return e.execCouponWake(ctx, item)
	case rule.ActMomentsPost:
		return e.execMomentsPost(item), nil
	case rule.ActPriceAdj:
		return e.execPriceAdj(ctx, item, in)
	case rule.ActGroupbuy:
		return e.execGroupbuy(ctx, item)
	case rule.ActRechargePromo:
		// CONTRACT §12.1 显式延后: recharge_promo real execution is M4.
		return nil, ErrActionNotExecutable
	default:
		return nil, ErrActionNotExecutable
	}
}

// execGroupSeckill: issue coupon + group-broadcast on wework_group / recall_30
// (CONTRACT §12.3.9 ActGroupSeckill). Coupon first (attribution), then reach;
// the Send outcome drives executed / blocked / failed attribution.
func (e *Executor) execGroupSeckill(ctx context.Context, item *strategy.StrategyItem) (*ExecResult, error) {
	if e.coupons == nil || e.reacher == nil {
		return nil, ErrPortNotConfigured
	}
	if !e.marginOkForCoupon(item) {
		return nil, ErrMarginFloor
	}
	memberID := paramInt64(item.Params, "member_id")
	couponID := paramInt64(item.Params, "coupon_id")
	if memberID == 0 || couponID == 0 {
		// aiengine 修复②: a coupon-bearing item that reached exec without a
		// resolved member/coupon is an honest audience-resolution gap, not a
		// malformed item — surface blocked(audience_unresolved) so the boss gets
		// actionable feedback instead of the misleading invalid_item.
		return nil, ErrAudienceUnresolved
	}
	ref, err := e.coupons.Issue(ctx, memberID, couponID, item.StrategyID, e.expireAt(item.Params))
	if err != nil {
		return nil, err
	}
	sendRef, err := e.reacher.Send(ctx, storeIDFromCtx(ctx), memberID, item.StrategyID,
		channelWeworkGroup, tplRecall30, sendVars(item))
	if err != nil {
		return nil, err
	}
	return classifySend(item.ID, sendRef, ref.MemberCouponID), nil
}

// execCouponWake: issue coupon + 1v1 reach on wework_1v1. Template is chosen by
// ReasonType (CONTRACT §12.3.9 ActCouponWake tpl table) — the ONLY place exec
// reads ReasonType.
func (e *Executor) execCouponWake(ctx context.Context, item *strategy.StrategyItem) (*ExecResult, error) {
	if e.coupons == nil || e.reacher == nil {
		return nil, ErrPortNotConfigured
	}
	if !e.marginOkForCoupon(item) {
		return nil, ErrMarginFloor
	}
	memberID := paramInt64(item.Params, "member_id")
	couponID := paramInt64(item.Params, "coupon_id")
	if memberID == 0 || couponID == 0 {
		// aiengine 修复②: see execGroupSeckill — honest audience_unresolved.
		return nil, ErrAudienceUnresolved
	}
	ref, err := e.coupons.Issue(ctx, memberID, couponID, item.StrategyID, e.expireAt(item.Params))
	if err != nil {
		return nil, err
	}
	sendRef, err := e.reacher.Send(ctx, storeIDFromCtx(ctx), memberID, item.StrategyID,
		channelWeworkOneV, tplForWake(item.ReasonType), sendVars(item))
	if err != nil {
		return nil, err
	}
	return classifySend(item.ID, sendRef, ref.MemberCouponID), nil
}

// execMomentsPost: M3-S1 audit-only (CONTRACT §12.1 显式延后: moments real-send
// needs 海报/COS, M3-S2). The copy is already on the item; no port is driven,
// RefID / TaskID stay 0.
func (e *Executor) execMomentsPost(item *strategy.StrategyItem) *ExecResult {
	return executed(item.ID, 0, 0)
}

// execPriceAdj: high-risk, requires boss confirmation + margin pre-check before
// calling the price port (CONTRACT §12.3.9 ActPriceAdj).
func (e *Executor) execPriceAdj(ctx context.Context, item *strategy.StrategyItem, in ExecuteInput) (*ExecResult, error) {
	if !in.Confirm {
		return nil, ErrConfirmRequired
	}
	if e.pricer == nil {
		return nil, ErrPortNotConfigured
	}
	costCents := paramInt64(item.Params, "cost_cents")
	priceCents := paramInt64(item.Params, "price_cents") // the NEW price
	if item.ProductID == 0 || priceCents == 0 {
		return nil, ErrInvalidItem
	}
	if err := margin.ValidatePriceAdj(e.guard, costCents, priceCents); err != nil {
		return nil, ErrMarginFloor
	}
	if err := e.pricer.UpdatePrice(ctx, item.ProductID, costCents, priceCents); err != nil {
		return nil, err
	}
	return executed(item.ID, 0, 0), nil
}

// execGroupbuy: launch a group-buy (CONTRACT §12.3.9 ActGroupbuy). Title falls
// back to the item Copy; RefID = GroupbuyID.
func (e *Executor) execGroupbuy(ctx context.Context, item *strategy.StrategyItem) (*ExecResult, error) {
	if e.groupbuy == nil {
		return nil, ErrPortNotConfigured
	}
	priceCents := paramInt64(item.Params, "price_cents")
	required := int(paramInt64(item.Params, "required_count"))
	validHours := int(paramInt64(item.Params, "valid_hours"))
	if item.ProductID == 0 || priceCents == 0 || required <= 0 {
		return nil, ErrInvalidItem
	}
	ref, err := e.groupbuy.Launch(ctx, storeIDFromCtx(ctx), item.ProductID,
		groupbuyTitle(item), priceCents, required, validHours, item.StrategyID)
	if err != nil {
		return nil, err
	}
	return executed(item.ID, ref.GroupbuyID, 0), nil
}

// classifySend maps a reach SendRef to an ExecResult. sent → executed (coupon
// RefID + task id); blocked → blocked(BlockedReason); failed → failed.
func classifySend(itemID int64, ref *SendRef, couponRefID int64) *ExecResult {
	if ref == nil {
		return executed(itemID, couponRefID, 0)
	}
	switch ref.Status {
	case statusSent:
		return executed(itemID, couponRefID, ref.TaskID)
	case statusFailed:
		return failed(itemID, ReasonSendFailed, ref.TaskID, couponRefID)
	default: // blocked / pending / unknown → blocked
		reason := ref.BlockedReason
		if reason == "" {
			reason = ReasonBlocked
		}
		return blocked(itemID, reason, ref.TaskID, couponRefID)
	}
}

// marginOkForCoupon is the third margin defense-in-depth check for coupon
// actions (CONTRACT §12.5.4). It builds a CouponRule + ProductView from the
// item Params when cost/price are present; when they are absent it defers to
// marketing.Issue (the final authority) and returns true. The shared guard
// config is the same one rule / agent use (injected via NewExecutor).
func (e *Executor) marginOkForCoupon(item *strategy.StrategyItem) bool {
	costCents := paramInt64(item.Params, "cost_cents")
	priceCents := paramInt64(item.Params, "price_cents")
	if costCents <= 0 || priceCents <= 0 {
		return true // no cost/price view → defer to marketing.Issue
	}
	cr := margin.CouponRule{
		Type:             stringParam(item.Params, "coupon_type", stringParam(item.Params, "type", couponTypeDiscount)),
		ThresholdCents:   paramInt64(item.Params, "threshold_cents"),
		OffCents:         paramInt64(item.Params, "off_cents"),
		DiscountPermille: paramInt64(item.Params, "discount_permille"),
	}
	products := []margin.ProductView{{ID: item.ProductID, CostCents: costCents, PriceCents: priceCents}}
	return margin.ValidateCoupon(e.guard, cr, products) == nil
}

// expireAt derives the coupon expiry timestamp from Params["expire_hours"]
// relative to the executor clock. nil / <=0 → no expiry.
func (e *Executor) expireAt(m map[string]any) *time.Time {
	hours := paramHours(m, "expire_hours")
	if hours <= 0 {
		return nil
	}
	t := e.now().Add(time.Duration(hours) * time.Hour)
	return &t
}

// blockedReason maps an intrinsic-block sentinel to its ExecResult.Reason
// string. ok is false for real (non-block) errors.
func blockedReason(err error) (string, bool) {
	switch {
	case errors.Is(err, ErrMarginFloor):
		return ReasonMarginFloor, true
	case errors.Is(err, ErrConfirmRequired):
		return ReasonConfirmRequired, true
	case errors.Is(err, ErrActionNotExecutable):
		return ReasonNotExecutable, true
	case errors.Is(err, ErrPortNotConfigured):
		return ReasonNotConfigured, true
	case errors.Is(err, ErrInvalidItem):
		return ReasonInvalidItem, true
	case errors.Is(err, ErrAudienceUnresolved):
		return ReasonAudienceUnresolved, true
	}
	return "", false
}

// storeIDFromCtx resolves the boss's store scope from ctx (the boss session
// carries store). Falls back to 0 when unset; the downstream adapter re-derives
// from ctx if needed.
func storeIDFromCtx(ctx context.Context) int64 {
	if mc, ok := multitenancy.FromContext(ctx); ok {
		return mc.StoreID
	}
	return 0
}

// executed / blocked / failed are the three ExecResult constructors. Status
// values use the strategy.ItemXxx constants (CONTRACT §12.3.9 alignment).
func executed(itemID, refID, taskID int64) *ExecResult {
	return &ExecResult{StrategyItemID: itemID, Status: strategy.ItemExecuted, RefID: refID, TaskID: taskID}
}
func blocked(itemID int64, reason string, taskID, refID int64) *ExecResult {
	return &ExecResult{StrategyItemID: itemID, Status: strategy.ItemBlocked, Reason: reason, TaskID: taskID, RefID: refID}
}
func failed(itemID int64, reason string, taskID, refID int64) *ExecResult {
	return &ExecResult{StrategyItemID: itemID, Status: strategy.ItemFailed, Reason: reason, TaskID: taskID, RefID: refID}
}
