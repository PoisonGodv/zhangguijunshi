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

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/aiengine"
	"github.com/zhangguijunshi/backend/internal/aiengine/attribution"
	"github.com/zhangguijunshi/backend/internal/aiengine/exec"
	"github.com/zhangguijunshi/backend/internal/strategy"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// AIEngineHandler serves the AI engine endpoints.
type AIEngineHandler struct {
	pipeline *aiengine.Pipeline
	executor *exec.Executor
	strategy *strategy.Service
	// M3-S3 attribution read API (CONTRACT §14.7). nil disables the
	// /strategies/:id/attribution endpoint cleanly.
	attr attribution.Repo
}

// NewAIEngineHandler constructs an AIEngineHandler. Any of the four may be nil
// in a minimal test fixture; the corresponding endpoints are then not wired.
func NewAIEngineHandler(p *aiengine.Pipeline, ex *exec.Executor, s *strategy.Service, attrRepo attribution.Repo) *AIEngineHandler {
	return &AIEngineHandler{pipeline: p, executor: ex, strategy: s, attr: attrRepo}
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
	RevenueCNY   *float64 `json:"revenue_cny,omitempty"`      // 增收估算, M3-S3 best-effort null
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
// revenue_cny is M3-S3 best-effort null — real revenue attribution needs order
// amount linkage and lands in M4 (CONTRACT §14.8).
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
	s, err := h.strategy.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, strategy.ErrNotFound) {
			httpx.Fail(c, errcode.ErrStrategyNotFound)
			return
		}
		httpx.FailError(c, err)
		return
	}
	items := make([]attributionItemDTO, 0, len(s.Items))
	for _, it := range s.Items {
		metrics, _ := h.attr.LatestPerMetric(c.Request.Context(), s.TenantID, it.ID)
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
		items = append(items, dto)
	}
	httpx.OK(c, attributionResponseDTO{
		StrategyID: s.ID,
		ForDate:    s.ForDate,
		Items:      items,
	})
}
