package handler

// MarketingHandler exposes the boss-surface coupon endpoints (M2-S1, CONTRACT
// §10.3 BE-B-02): coupon-template CRUD, status transitions, member-coupon
// issuance / redemption, and member-wallet listing. Tenant/store/operator come
// from the verified request context, never the request body.

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// MarketingHandler serves marketing endpoints.
type MarketingHandler struct {
	svc *marketing.Service
}

// NewMarketingHandler constructs a MarketingHandler.
func NewMarketingHandler(svc *marketing.Service) *MarketingHandler {
	return &MarketingHandler{svc: svc}
}

type createCouponRequest struct {
	StoreID    int64                  `json:"store_id"`
	Name       string                 `json:"name" binding:"required"`
	Type       string                 `json:"type" binding:"required"`
	Params     marketing.CouponParams `json:"params"`
	ValidFrom  *time.Time             `json:"valid_from"`
	ValidTo    *time.Time             `json:"valid_to" binding:"required"`
	TotalQuota int64                  `json:"total_quota"`
}

// CreateCoupon creates a draft coupon template.
// POST /v1/boss/marketing/coupons
func (h *MarketingHandler) CreateCoupon(c *gin.Context) {
	var req createCouponRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var vf time.Time
	if req.ValidFrom != nil {
		vf = *req.ValidFrom
	}
	in := marketing.CreateCouponInput{
		StoreID:    storeScope(c, req.StoreID),
		Name:       req.Name,
		Type:       req.Type,
		Params:     req.Params,
		ValidFrom:  vf,
		ValidTo:    *req.ValidTo,
		TotalQuota: req.TotalQuota,
	}
	if mc, ok := operatorUserID(c); ok {
		in.CreatedBy = mc
	}
	coupon, err := h.svc.CreateCoupon(c.Request.Context(), in)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, coupon)
}

type updateStatusRequest struct {
	Status string `json:"status" binding:"required"`
}

// UpdateStatus transitions a coupon's status (draft→active, active↔paused, →ended).
// PATCH /v1/boss/marketing/coupons/:id/status
func (h *MarketingHandler) UpdateStatus(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req updateStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	coupon, err := h.svc.UpdateStatus(c.Request.Context(), id, req.Status)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, coupon)
}

// GetCoupon loads one coupon template.
// GET /v1/boss/marketing/coupons/:id
func (h *MarketingHandler) GetCoupon(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	coupon, err := h.svc.GetCoupon(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, marketing.ErrNotFound) {
			httpx.Fail(c, errcode.Err{Code: 40403, Message: "券不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, coupon)
}

// ListCoupons lists coupon templates with optional filters.
// GET /v1/boss/marketing/coupons?status=&type=&store_id=&limit=&offset=
func (h *MarketingHandler) ListCoupons(c *gin.Context) {
	f := marketing.CouponFilter{
		StoreID: queryInt64(c, "store_id"),
		Status:  c.Query("status"),
		Type:    c.Query("type"),
		Limit:   int(queryInt64(c, "limit")),
		Offset:  int(queryInt64(c, "offset")),
	}
	coupons, err := h.svc.ListCoupons(c.Request.Context(), f)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, coupons)
}

type issueRequest struct {
	MemberID   int64      `json:"member_id" binding:"required,min=1"`
	CouponID   int64      `json:"coupon_id" binding:"required,min=1"`
	Source     string     `json:"source"`
	StrategyID int64      `json:"strategy_id"`
	ExpireAt   *time.Time `json:"expire_at"`
}

// Issue creates a member-coupon instance (manual or attributed).
// POST /v1/boss/marketing/coupons/issue
func (h *MarketingHandler) Issue(c *gin.Context) {
	var req issueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	mc, err := h.svc.Issue(c.Request.Context(), marketing.IssueInput{
		MemberID:   req.MemberID,
		CouponID:   req.CouponID,
		Source:     req.Source,
		StrategyID: req.StrategyID,
		ExpireAt:   req.ExpireAt,
	})
	if err != nil {
		h.failMarketing(c, err)
		return
	}
	httpx.OK(c, mc)
}

type redeemCouponRequest struct {
	MemberCouponID int64 `json:"member_coupon_id" binding:"required,min=1"`
	RefOrderID     int64 `json:"ref_order_id"`
}

// Redeem transitions a member-coupon unused→used.
// POST /v1/boss/marketing/coupons/redeem
func (h *MarketingHandler) Redeem(c *gin.Context) {
	var req redeemCouponRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	mc, err := h.svc.Redeem(c.Request.Context(), marketing.RedeemInput{
		MemberCouponID: req.MemberCouponID,
		RefOrderID:     req.RefOrderID,
	})
	if err != nil {
		h.failMarketing(c, err)
		return
	}
	httpx.OK(c, mc)
}

// ListMemberCoupons lists a member's coupons.
// GET /v1/boss/marketing/members/:id/coupons?status=
func (h *MarketingHandler) ListMemberCoupons(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	list, err := h.svc.ListMemberCoupons(c.Request.Context(), id, c.Query("status"))
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, list)
}

// failMarketing maps marketing domain errors to HTTP error codes.
func (h *MarketingHandler) failMarketing(c *gin.Context, err error) {
	switch {
	case errors.Is(err, marketing.ErrNotFound):
		httpx.Fail(c, errcode.Err{Code: 40403, Message: "券不存在", HTTP: 404})
	case errors.Is(err, marketing.ErrQuotaExhausted):
		httpx.Fail(c, errcode.ErrCouponQuota)
	case errors.Is(err, marketing.ErrCouponInactive):
		httpx.Fail(c, errcode.ErrCouponInactive)
	case errors.Is(err, marketing.ErrCouponExpired):
		httpx.Fail(c, errcode.ErrCouponExpired)
	case errors.Is(err, marketing.ErrCouponUsed):
		httpx.Fail(c, errcode.ErrCouponUsed)
	case errors.Is(err, margin.ErrBelowMarginFloor):
		httpx.Fail(c, errcode.ErrMarginGuard)
	default:
		httpx.FailError(c, err)
	}
}

// operatorUserID extracts the operator (boss) user id from the verified ctx.
func operatorUserID(c *gin.Context) (int64, bool) {
	mc, ok := multitenancy.FromContext(c.Request.Context())
	if !ok || mc.UserID <= 0 {
		return 0, false
	}
	return mc.UserID, true
}
