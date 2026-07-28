package handler

// MembershipHandler exposes the boss-surface member endpoints (M1-S2,
// CONTRACT §9.3 BE-B-01 + BE-A-07): join, get, issue dynamic member code, and
// redeem (the 0.5s member-code close-the-loop). Tenant/store/operator come from
// the verified request context, never the request body.

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// MembershipHandler serves membership endpoints.
type MembershipHandler struct {
	svc *membership.Service
}

// NewMembershipHandler constructs a MembershipHandler.
func NewMembershipHandler(svc *membership.Service) *MembershipHandler {
	return &MembershipHandler{svc: svc}
}

type joinRequest struct {
	StoreID   int64  `json:"store_id"`
	PhoneHash string `json:"phone_hash"`
	WxOpenID  string `json:"wx_openid"`
	Name      string `json:"name"`
}

// Join registers (or reuses) a member and opens a balance account.
// POST /v1/boss/membership/members
func (h *MembershipHandler) Join(c *gin.Context) {
	var req joinRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	m, err := h.svc.Join(c.Request.Context(), membership.JoinInput{
		StoreID:   storeScope(c, req.StoreID),
		PhoneHash: req.PhoneHash,
		WxOpenID:  req.WxOpenID,
		Name:      req.Name,
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, m)
}

// GetMember loads one member within the caller's tenant.
// GET /v1/boss/membership/members/:id
func (h *MembershipHandler) GetMember(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	m, err := h.svc.GetMember(c.Request.Context(), id)
	if err != nil {
		if err == membership.ErrNotFound {
			httpx.Fail(c, errcode.Err{Code: 40402, Message: "会员不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, m)
}

// IssueCode issues a short-lived dynamic member code for a member.
// POST /v1/boss/membership/members/:id/code
func (h *MembershipHandler) IssueCode(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	code, err := h.svc.IssueCode(c.Request.Context(), id)
	if err != nil {
		switch err {
		case membership.ErrNotFound:
			httpx.Fail(c, errcode.Err{Code: 40402, Message: "会员不存在", HTTP: 404})
		case membership.ErrMemberBlocked:
			httpx.Fail(c, errcode.Err{Code: 42302, Message: "会员已被冻结", HTTP: 423})
		default:
			httpx.FailError(c, err)
		}
		return
	}
	httpx.OK(c, gin.H{"code": code, "member_id": id})
}

type redeemRequest struct {
	StoreID      int64  `json:"store_id"`
	Code         string `json:"code" binding:"required"`
	ConsumeCents int64  `json:"consume_cents"`
	EarnPoints   int64  `json:"earn_points"`
	RefOrderID   int64  `json:"ref_order_id"`
}

// Redeem closes the member-code loop: resolve the code, optionally deduct
// stored value and/or accrue points, all atomically within the caller's tenant.
// POST /v1/boss/membership/redeem
func (h *MembershipHandler) Redeem(c *gin.Context) {
	var req redeemRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var operatorID int64
	if mc, ok := multitenancy.FromContext(c.Request.Context()); ok {
		operatorID = mc.UserID
	}
	res, err := h.svc.Redeem(c.Request.Context(), membership.RedeemInput{
		StoreID:      storeScope(c, req.StoreID),
		OperatorID:   operatorID,
		RefOrderID:   req.RefOrderID,
		Code:         req.Code,
		ConsumeCents: req.ConsumeCents,
		EarnPoints:   req.EarnPoints,
	})
	if err != nil {
		switch err {
		case membership.ErrCodeInvalid:
			httpx.Fail(c, errcode.Err{Code: 40003, Message: "会员码无效或已过期", HTTP: 400})
		case membership.ErrInsufficientBalance:
			httpx.Fail(c, errcode.ErrBalanceShort)
		case membership.ErrMemberBlocked:
			httpx.Fail(c, errcode.Err{Code: 42302, Message: "会员已被冻结", HTTP: 423})
		default:
			httpx.FailError(c, err)
		}
		return
	}
	httpx.OK(c, res)
}

type rechargeRequest struct {
	AmountCents int64 `json:"amount_cents" binding:"required,min=1"`
}

// Recharge tops up a member's stored value (manual / admin book-keeping entry).
// 反二清: the fund path is prepay(subject=recharge) → WeChat notify →
// paymentFulfiller → Recharge; this endpoint is the manual/testing entry that
// credits the book balance directly. Regional cap is enforced (errcode 60003).
// POST /v1/boss/membership/members/:id/recharge
func (h *MembershipHandler) Recharge(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req rechargeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var operatorID int64
	if mc, ok := multitenancy.FromContext(c.Request.Context()); ok {
		operatorID = mc.UserID
	}
	res, err := h.svc.Recharge(c.Request.Context(), membership.RechargeInput{
		MemberID:    id,
		AmountCents: req.AmountCents,
		OperatorID:  operatorID,
	})
	if err != nil {
		switch err {
		case membership.ErrNotFound:
			httpx.Fail(c, errcode.Err{Code: 40402, Message: "会员不存在", HTTP: 404})
		case membership.ErrMemberBlocked:
			httpx.Fail(c, errcode.Err{Code: 42302, Message: "会员已被冻结", HTTP: 423})
		default:
			httpx.FailError(c, err)
		}
		return
	}
	httpx.OK(c, res)
}

type regionCapRequest struct {
	RegionCapCents int64 `json:"region_cap_cents"`
}

// SetRegionCap sets the member's stored-value regional cap (compliance for
// 上海/江苏 etc.; 0 = unlimited).
// POST /v1/boss/membership/members/:id/region-cap
func (h *MembershipHandler) SetRegionCap(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req regionCapRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	if err := h.svc.SetRegionCap(c.Request.Context(), id, req.RegionCapCents); err != nil {
		if err == membership.ErrNotFound {
			httpx.Fail(c, errcode.Err{Code: 40402, Message: "会员不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"ok": true})
}
