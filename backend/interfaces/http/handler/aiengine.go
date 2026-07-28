package handler

// AIEngineHandler exposes the boss-surface AI engine endpoints (M3-S1, CONTRACT
// §12.7): generate today's 3-thing advice, list/get strategies, one-click
// execute a strategy item, and record boss feedback. Tenant/store come from the
// verified request context, never the request body.
//
// A blocked execution is NOT an error: exec.Executor.Run returns an ExecResult
// with Status=blocked + Reason, and this handler passes it through as a 200 OK
// data payload so the client can branch (executed / blocked / failed). Only
// hard errors (unknown strategy / param / no tenant / system fault) map to
// errcode failures. Likewise a degraded Advice (budget / llm / no_provider /
// cold_start) returns 200 + Advice.Degrade.

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/aiengine"
	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// AIEngineHandler serves the AI engine endpoints.
type AIEngineHandler struct {
	pipeline *aiengine.Pipeline
	executor *exec.Executor
	strategy *strategy.Service
	// M3-S3 attribution read API (CONTRACT §14.7). nil disables the
	// /strategies/:id/attribution endpoint cleanly.
	attr attribution.Repo
	// M3-S3 收尾 best-effort revenue_cny 估算 (CONTRACT §14.9). mkt resolves the
	// strategy-touched member set; bill sums their paid-order total in the
	// [ExecutedAt, now] window. nil either → revenue_cny surfaces as null.
	mkt  *marketing.Service
	bill *billing.Service
}

// NewAIEngineHandler constructs an AIEngineHandler. Any of the six may be nil
// in a minimal test fixture; the corresponding endpoints / best-effort fields
// are then not wired.
func NewAIEngineHandler(p *aiengine.Pipeline, ex *exec.Executor, s *strategy.Service, attrRepo attribution.Repo, mktSvc *marketing.Service, billSvc *billing.Service) *AIEngineHandler {
	return &AIEngineHandler{pipeline: p, executor: ex, strategy: s, attr: attrRepo, mkt: mktSvc, bill: billSvc}
}

type todayRequest struct {
	Date string `json:"date"`
}

// Today generates (or returns cached) today's 3-thing advice.
// POST /v1/boss/aiengine/today
func (h *AIEngineHandler) Today(c *gin.Context) {
	if h.pipeline == nil {
		httpx.Fail(c, errcode.ErrInternal)
		return
	}
	var req todayRequest
	_ = c.ShouldBindJSON(&req) // body optional; date defaults to today inside pipeline
	advice, err := h.pipeline.Run(c.Request.Context())
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, advice)
}

// ListStrategies lists strategies for the ctx tenant.
// GET /v1/boss/aiengine/strategies?date=&status=
func (h *AIEngineHandler) ListStrategies(c *gin.Context) {
	if h.strategy == nil {
		httpx.Fail(c, errcode.ErrInternal)
		return
	}
	list, err := h.strategy.List(c.Request.Context(), strategy.StrategyFilter{
		Date:   c.Query("date"),
		Status: c.Query("status"),
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, list)
}

// GetStrategy loads one strategy (with items) by id.
// GET /v1/boss/aiengine/strategies/:id
func (h *AIEngineHandler) GetStrategy(c *gin.Context) {
	if h.strategy == nil {
		httpx.Fail(c, errcode.ErrInternal)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	s, err := h.strategy.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, strategy.ErrNotFound) {
			httpx.Fail(c, errcode.ErrStrategyNotFound)
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, s)
}

type executeRequest struct {
	Confirm bool `json:"confirm"`
}

// ExecuteItem runs the one-click executor on a strategy item.
// POST /v1/boss/aiengine/strategies/:id/items/:itemID/execute
// blocked is NOT failure → 200 + ExecResult{status:"blocked"}.
func (h *AIEngineHandler) ExecuteItem(c *gin.Context) {
	if h.executor == nil {
		httpx.Fail(c, errcode.ErrInternal)
		return
	}
	itemID, err := strconv.ParseInt(c.Param("itemID"), 10, 64)
	if err != nil || itemID <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req executeRequest
	_ = c.ShouldBindJSON(&req) // body optional (confirm defaults false)
	res, err := h.executor.Run(c.Request.Context(), exec.ExecuteInput{
		StrategyItemID: itemID,
		Confirm:        req.Confirm,
	})
	if err != nil {
		if errors.Is(err, strategy.ErrNotFound) {
			httpx.Fail(c, errcode.ErrStrategyNotFound)
			return
		}
		httpx.FailError(c, err)
		return
	}
	// blocked / executed / failed all surface as 200 + ExecResult.Status.
	httpx.OK(c, res)
}

type feedbackRequest struct {
	Useful bool `json:"useful"`
}

// RecordFeedback records the boss's 👍/👎 on a strategy item (rewards the rule
// weights in a later milestone; M3-S1 just persists).
// POST /v1/boss/aiengine/strategies/:id/items/:itemID/feedback
func (h *AIEngineHandler) RecordFeedback(c *gin.Context) {
	if h.strategy == nil {
		httpx.Fail(c, errcode.ErrInternal)
		return
	}
	itemID, err := strconv.ParseInt(c.Param("itemID"), 10, 64)
	if err != nil || itemID <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req feedbackRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	if _, err := h.strategy.RecordFeedback(c.Request.Context(), itemID, req.Useful); err != nil {
		if errors.Is(err, strategy.ErrNotFound) {
			httpx.Fail(c, errcode.ErrStrategyNotFound)
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"ok": true})
}

// attributionItemDTO is one item's aggregated attribution for the boss card.
type attributionItemDTO struct {
	Rank         int      `json:"rank"`
	ActionType   string   `json:"action_type"`
	ReasonType   string   `json:"reason_type"`
	CouponRedeem *float64 `json:"coupon_redeem,omitempty"`    // 核销率 0-1, null=无券数据
	VisitUplift  *float64 `json:"visit_uplift,omitempty"`     // 到店反推增量, null=无数据
	GroupbuySucc *float64 `json:"groupbuy_success,omitempty"` // 拼团成团率 0-1, null=无数据
	Repurchase   *float64 `json:"repurchase,omitempty"`       // 触达会员复购率 0-1, M3-S3 best-effort null
	RevenueCNY   *float64 `json:"revenue_cny,omitempty"`      // 增收估算(元), M3-S3 best-effort null
}

// attributionResponseDTO is the dashboard payload for one strategy.
type attributionResponseDTO struct {
	StrategyID int64                `json:"strategy_id"`
	ForDate    string               `json:"for_date"`
	Items      []attributionItemDTO `json:"items"`
}

// GetAttribution aggregates the latest strategy_attribution rows per item into
// a boss-readable dashboard payload (CONTRACT §14.7 / FE-ADMIN-03 read API).
// GET /v1/boss/aiengine/strategies/:id/attribution
//
// Missing metrics surface as null (the boss sees "—" not a misleading 0);
// cross-tenant access returns 40404 (indistinguishable from absence, ARCH-01).
// revenue_cny is M3-S3 best-effort: the handler resolves the strategy-touched
// members via marketing.MembersByStrategy, then sums their paid-order total in
// the [item.ExecutedAt, now] window via billing.OrderTotalByMembers. nil
// mkt/bill, nil ExecutedAt, or any upstream error → revenue_cny is null (no
// panic, no cross-tenant leakage — both ports resolve tenant from the verified
// ctx via multitenancy). moments_engage remains null (no data source, M4).
func (h *AIEngineHandler) GetAttribution(c *gin.Context) {
	if h.strategy == nil || h.attr == nil {
		httpx.Fail(c, errcode.ErrInternal)
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	ctx := c.Request.Context()
	s, err := h.strategy.Get(ctx, id)
	if err != nil {
		if errors.Is(err, strategy.ErrNotFound) {
			httpx.Fail(c, errcode.ErrStrategyNotFound)
			return
		}
		httpx.FailError(c, err)
		return
	}
	// System-sourced identity so the marketing/billing ports (which call
	// multitenancy.EnforceTenant) re-derive the same tenant the boss context
	// already verified. Aligned with the adapter wiring in cmd/api.
	mctx := multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: s.TenantID,
		Source:   multitenancy.SourceSystem,
	})
	now := time.Now()
	items := make([]attributionItemDTO, 0, len(s.Items))
	for _, it := range s.Items {
		metrics, _ := h.attr.LatestPerMetric(ctx, s.TenantID, it.ID)
		dto := attributionItemDTO{
			Rank:       it.Rank,
			ActionType: it.ActionType,
			ReasonType: it.ReasonType,
		}
		if v, ok := metrics[attribution.MetricCouponRedeem]; ok {
			dto.CouponRedeem = &v
		}
		if v, ok := metrics[attribution.MetricVisitUplift]; ok {
			dto.VisitUplift = &v
		}
		if v, ok := metrics[attribution.MetricGroupbuySuccess]; ok {
			dto.GroupbuySucc = &v
		}
		if v, ok := metrics[attribution.MetricRepurchase]; ok {
			dto.Repurchase = &v
		}
		// revenue_cny best-effort (M3-S3 收尾). Only computable when both
		// ports are wired, the item was executed (ExecutedAt != nil), and the
		// strategy touched ≥1 member. Errors degrade to null, never panic.
		if h.mkt != nil && h.bill != nil && it.StrategyID > 0 && it.ExecutedAt != nil {
			if memberIDs, mErr := h.mkt.MembersByStrategy(mctx, s.TenantID, it.StrategyID); mErr == nil && len(memberIDs) > 0 {
				if totalCents, bErr := h.bill.OrderTotalByMembers(mctx, memberIDs, *it.ExecutedAt, now); bErr == nil && totalCents > 0 {
					v := float64(totalCents) / 100.0
					dto.RevenueCNY = &v
				}
			}
		}
		items = append(items, dto)
	}
	httpx.OK(c, attributionResponseDTO{
		StrategyID: s.ID,
		ForDate:    s.ForDate,
		Items:      items,
	})
}
