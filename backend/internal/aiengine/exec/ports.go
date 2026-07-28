// Package exec implements the ActionExecutor (BE-B-11) — the "一键执行" hub of
// the product's core selling point ("AI 今日营销军师：今日 3 件事 + 一键执行").
//
// Given a strategy_item_id the Executor loads the item from the strategy repo
// (never trusting caller-supplied detail), dispatches on the item's ActionType
// (the 6 execution channels defined in the rule package) to one of 6 handlers,
// and drives four anti-corruption ports that the main thread adapts to the
// concrete domains (marketing / reach / catalog / groupbuy). exec imports NO
// concrete domain package — only strategy (item + state machine), margin (the
// pure-function guardrail, third defense-in-depth check) and rule (ActionType
// constants for dispatch). Channel / template strings mirror reach's values as
// bare local constants so exec stays decoupled.
//
// Blocked is NOT failure (CONTRACT §12.5.7, §12.7). A compliance block — margin
// floor, frequency circuit, content filter, unsubscribe, subscription quota,
// missing boss confirmation, an action whose real execution is deferred, or an
// unconfigured port — yields ExecResult{Status: "blocked"} + the item landing
// in strategy.ItemBlocked, returned with a nil error so the boss gets
// actionable feedback over HTTP 200. A real system error from a port yields
// ExecResult{Status:"failed"} (or a returned Go error) + ItemFailed.
//
// ActionType is the sole dispatch key (CONTRACT §12.5.9: action_type two-layer
// non-drift). ReasonType is consulted ONLY inside coupon_wake's template
// selection, never for dispatch.
package exec

import (
	"context"
	"errors"
	"time"
)

// CouponRef is the issuance outcome referenced for attribution.
type CouponRef struct {
	MemberCouponID int64
	MemberID       int64
}

// CouponIssuer adapts marketing.Service.Issue. Source is fixed "ai_strategy";
// strategyID is the attribution primary key. expireAt may be nil (no expiry).
//
// Issue is the second margin guardrail site (marketing.Issue internally runs
// the catalog guard); exec's own ValidateCoupon pre-check is the third,
// best-effort defense-in-depth layer (CONTRACT §12.5.4).
type CouponIssuer interface {
	Issue(ctx context.Context, memberID, couponID, strategyID int64, expireAt *time.Time) (*CouponRef, error)
}

// SendRef mirrors reach.ReachTask's interception semantics. A blocked send is
// surfaced here (NOT as a Go error) so the executor can attribute it without
// mistaking compliance for a system fault.
type SendRef struct {
	TaskID        int64
	Status        string // sent|blocked|failed
	BlockedReason string // freq_circuit|content_filter|unsubscribed|subscribe_quota (non-empty when Status==blocked)
}

// MessageSender adapts reach.Service.Send. channel / tplCode / vars are chosen
// by the executor per ActionType (and per ReasonType for coupon_wake's tpl).
type MessageSender interface {
	Send(ctx context.Context, storeID, memberID, strategyID int64, channel, tplCode string, vars map[string]string) (*SendRef, error)
}

// PriceUpdater adapts catalog.Service.UpdatePrice. High-risk action: the
// executor pre-checks the margin floor AND requires boss confirmation before
// calling this (CONTRACT §12.3.9 ActPriceAdj).
type PriceUpdater interface {
	UpdatePrice(ctx context.Context, productID, costCents, priceCents int64) error
}

// GroupbuyRef is the launch outcome referenced for attribution.
type GroupbuyRef struct {
	GroupbuyID int64
}

// GroupbuyOpener adapts groupbuy.Service.Create. strategyID is the attribution
// key (M2-S2 groupbuy rows carry no strategy_id — only the ID is returned).
type GroupbuyOpener interface {
	Launch(ctx context.Context, storeID, productID int64, title string, priceCents int64, requiredCount, validHours int, strategyID int64) (*GroupbuyRef, error)
}

// ExecResult is the one-click execution outcome. Status is one of
// executed|blocked|failed, carrying the strategy.ItemExecuted / ItemBlocked /
// ItemFailed values. Reason carries the blocked/failed cause. TaskID is the
// reach task id for reach-typed actions; RefID is the MemberCouponID
// (group_seckill / coupon_wake) or GroupbuyID (groupbuy), by ActionType
// semantics; both are 0 when the action did not reach that stage.
type ExecResult struct {
	StrategyItemID int64  `json:"strategy_item_id"`
	Status         string `json:"status"`            // executed|blocked|failed
	Reason         string `json:"reason,omitempty"`  // margin_floor|freq_circuit|content_filter|unsubscribed|subscribe_quota|confirm_required|not_executable|not_configured|send_failed
	TaskID         int64  `json:"task_id,omitempty"` // reach.ReachTask.ID (reach-typed actions)
	RefID          int64  `json:"ref_id,omitempty"`  // MemberCouponID / GroupbuyID (by ActionType semantics)
}

// Reason* constants fill ExecResult.Reason. They are the canonical blocked /
// failed causes surfaced to the boss (CONTRACT §12.3.9 ExecResult.Reason).
const (
	ReasonMarginFloor     = "margin_floor"     // margin guard rejected the action pre-execution
	ReasonConfirmRequired = "confirm_required" // price_adj submitted without boss confirmation
	ReasonNotExecutable   = "not_executable"   // action real-execution deferred (recharge_promo → M4)
	ReasonNotConfigured   = "not_configured"   // required anti-corruption port not injected
	ReasonSendFailed      = "send_failed"      // reach returned a failed task (soft failure)
	ReasonBlocked         = "blocked"          // generic blocked (no specific reason carried)
	ReasonInvalidItem     = "invalid_item"     // strategy item missing required params (defensive)
)

// Sentinel errors. The handlers surface intrinsic blocks as these sentinels in
// the error channel; Executor.Run translates them into ExecResult{Status:
// blocked} (blocked is not failure → nil error to the caller). A non-sentinel
// error returned by a handler is treated as a real system fault (→ failed +
// returned to the caller). Tests / the HTTP layer may errors.Is against them.
var (
	// ErrMarginFloor: the margin guard rejected a discount / coupon / price
	// change pre-execution (third defense-in-depth check, CONTRACT §12.5.4).
	ErrMarginFloor = errors.New("exec: margin floor breached")
	// ErrConfirmRequired: a high-risk action (price_adj) was submitted without
	// boss confirmation.
	ErrConfirmRequired = errors.New("exec: confirmation required")
	// ErrActionNotExecutable: the action's real execution is deferred to a
	// later milestone (recharge_promo → M4, CONTRACT §12.1 显式延后).
	ErrActionNotExecutable = errors.New("exec: action not executable")
	// ErrPortNotConfigured: a required anti-corruption port was not injected
	// (main-thread wiring gap) — the handler declines cleanly instead of
	// nil-dereferencing.
	ErrPortNotConfigured = errors.New("exec: port not configured")
	// ErrInvalidItem: the strategy item is missing params required for the
	// action (defensive — a pipeline bug; surfaced as failed).
	ErrInvalidItem = errors.New("exec: strategy item missing required params")
)
