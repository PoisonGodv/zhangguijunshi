package handler

// WxshopHandler exposes the consumer (C 端) business endpoints behind
// /v1/wxshop (CONTRACT §16, MVP C端业务接口契约). Every handler resolves the
// caller from the verified JWT claims (openid) and the tenant/store from the
// verified request context (multitenancy), NEVER from the request body — the
// ARCH-01 anti-越权 rule. Cross-tenant access is indistinguishable from absence
// (40404 / empty), never a leak of another tenant's resource.
//
// memberID 约定（CONTRACT §16.2）: the consumer's member row is obtained by an
// IDEMPOTENT membership.Join keyed on (tenant, wx_openid) — the same identity
// the login flow proves. Join is idempotent, so the first C-side call enrols the
// member (and opens the zero-balance account); subsequent calls reuse it.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// WxshopHandler serves the consumer business endpoints.
type WxshopHandler struct {
	membership *membership.Service
	marketing  *marketing.Service
	groupbuy   *groupbuy.Service
	billing    *billing.Service // optional; nil → /orders degrades to empty list (minimal fixture)
	now        func() time.Time
}

// NewWxshopHandler constructs a WxshopHandler. membership/marketing/groupbuy
// are required for the endpoints that are wired; billing may be nil.
func NewWxshopHandler(mem *membership.Service, mkt *marketing.Service, gb *groupbuy.Service, bil *billing.Service) *WxshopHandler {
	return &WxshopHandler{membership: mem, marketing: mkt, groupbuy: gb, billing: bil, now: time.Now}
}

// memberCodeTTL mirrors the membership domain's dynamic-code ttl (60s). It is
// reported as expire_seconds so the mini-program can count down the nonce.
const memberCodeTTLSeconds = 60

// newcomerCouponName is the default 新人券 template title, auto-created when the
// tenant has no active "新人" coupon (CONTRACT §16.5). 满30减10, 7 天有效.
const newcomerCouponName = "新人见面礼 满30减10"

// memberFromClaims resolves the caller's member row via idempotent Join keyed on
// the verified openid. Returns nil (and writes the failure) when there is no
// verified identity — the ARCH-01 chokepoint.
func (h *WxshopHandler) memberFromClaims(c *gin.Context, phoneHash string) *membership.Member {
	claims := currentClaims(c)
	if claims == nil || claims.OpenID == "" {
		httpx.Fail(c, errcode.ErrTokenInvalid)
		return nil
	}
	m, err := h.membership.Join(c.Request.Context(), membership.JoinInput{
		WxOpenID:  claims.OpenID,
		PhoneHash: phoneHash, // empty when the endpoint does not collect a phone
	})
	if err != nil {
		httpx.FailError(c, err)
		return nil
	}
	return m
}

// couponDTO is the C-side coupon shape, field-aligned with the mini-program
// Coupon type (apps/consumer-mp/types/api.d.ts). All keys are snake_case.
type couponDTO struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	Type          string `json:"type"` // cash | discount | gift
	ValueText     string `json:"value_text"`
	ThresholdText string `json:"threshold_text,omitempty"`
	ScopeText     string `json:"scope_text,omitempty"`
	ExpireText    string `json:"expire_text"`
	Status        string `json:"status"` // unused | used | expired
}

// toCouponDTO renders a member-coupon + its template into the C-side DTO. The
// frontend Coupon.type is the narrower cash|discount|gift set, so the backend
// full_reduction template type folds into "cash" (a threshold off is still a
// fixed-amount reduction).
func toCouponDTO(mc *marketing.MemberCoupon, tpl *marketing.Coupon, now time.Time) couponDTO {
	d := couponDTO{Status: mc.Status}
	if tpl != nil {
		d.ID = tpl.ID
		d.Title = tpl.Name
		switch tpl.Type {
		case marketing.TypeDiscount:
			d.Type = "discount"
			d.ValueText = formatDiscount(tpl.Params.DiscountPermille)
			d.ThresholdText = "无门槛"
		case marketing.TypeGift:
			d.Type = "gift"
			d.ValueText = "赠品"
			d.ScopeText = "指定商品"
		default: // full_reduction | cash → 前端 "cash"
			d.Type = "cash"
			d.ValueText = formatYuan(tpl.Params.OffCents)
			if tpl.Params.ThresholdCents > 0 {
				d.ThresholdText = "满" + formatYuan(tpl.Params.ThresholdCents) + "可用"
			} else {
				d.ThresholdText = "无门槛"
			}
			if d.ScopeText == "" {
				d.ScopeText = "全场通用"
			}
		}
		d.ExpireText = tpl.ValidTo.Local().Format("2006-01-02") + " 到期"
	} else {
		d.ID = mc.CouponID
		d.ValueText = "券"
	}
	// A still-unused coupon past its expiry renders as expired (defence in depth;
	// the domain marks it on sweep, but the C 端 never shows a stale 可用券).
	if d.Status == marketing.MCStatusUnused {
		exp := mc.ExpireAt
		if exp == nil && tpl != nil {
			exp = &tpl.ValidTo
		}
		if exp != nil && exp.Before(now) {
			d.Status = marketing.MCStatusExpired
		}
	}
	return d
}

// formatYuan renders cents as a ¥ string (¥10 / ¥8.5).
func formatYuan(cents int64) string {
	if cents%100 == 0 {
		return fmt.Sprintf("¥%d", cents/100)
	}
	return fmt.Sprintf("¥%.2f", float64(cents)/100)
}

// formatDiscount renders a permille as a 折 string (850‰ → 8.5折).
func formatDiscount(permille int64) string {
	if permille%100 == 0 {
		return fmt.Sprintf("%d折", permille/100)
	}
	return fmt.Sprintf("%.1f折", float64(permille)/100)
}

// Coupons lists the caller's member-coupons (券包). status 缺省 unused.
// GET /v1/wxshop/coupons?status=
func (h *WxshopHandler) Coupons(c *gin.Context) {
	m := h.memberFromClaims(c, "")
	if m == nil {
		return
	}
	status := c.Query("status")
	if status == "" {
		status = marketing.MCStatusUnused
	}
	list, err := h.marketing.ListMemberCoupons(c.Request.Context(), m.ID, status)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	out := make([]couponDTO, 0, len(list))
	now := h.now()
	for _, mc := range list {
		tpl, err := h.marketing.GetCoupon(c.Request.Context(), mc.CouponID)
		if err != nil {
			tpl = nil // template gone (e.g. cross-tenant / deleted): degrade gracefully
		}
		out = append(out, toCouponDTO(mc, tpl, now))
	}
	httpx.OK(c, out)
}

// Profile returns the caller's member profile. balance_cents 降级为 0（见
// CONTRACT §16.4 TODO: membership 未暴露 GetAccount 公开方法，储值余额待补）.
// GET /v1/wxshop/member/profile
func (h *WxshopHandler) Profile(c *gin.Context) {
	m := h.memberFromClaims(c, "")
	if m == nil {
		return
	}
	httpx.OK(c, gin.H{
		"member_id":         m.ID,
		"phone_masked":      "", // 手机号仅存 hash,无法回显; C 端档案不展示手机号
		"level":             m.Level,
		"level_name":        memberLevelName(m.Level),
		"points":            m.Points,
		"balance_cents":     0, // TODO(CONTRACT §16.4): 接 membership.GetAccount 后返回真实储值余额
		"total_spent_cents": m.TotalSpentCents,
		"tenant_id":         m.TenantID,
		"store_id":          m.StoreID,
	})
}

// memberLevelName maps the domain level to a display name.
func memberLevelName(level string) string {
	switch level {
	case "vip":
		return "银卡会员"
	case "blackcard":
		return "金卡会员"
	default:
		return "普通会员"
	}
}

// MemberCode issues a short-lived dynamic member code (nonce) for the caller.
// POST /v1/wxshop/member/code
func (h *WxshopHandler) MemberCode(c *gin.Context) {
	m := h.memberFromClaims(c, "")
	if m == nil {
		return
	}
	code, err := h.membership.IssueCode(c.Request.Context(), m.ID)
	if err != nil {
		switch {
		case errors.Is(err, membership.ErrNotFound):
			httpx.Fail(c, errcode.Err{Code: 40402, Message: "会员不存在", HTTP: 404})
		case errors.Is(err, membership.ErrMemberBlocked):
			httpx.Fail(c, errcode.Err{Code: 42302, Message: "会员已被冻结", HTTP: 423})
		default:
			httpx.FailError(c, err)
		}
		return
	}
	httpx.OK(c, gin.H{"code": code, "expire_seconds": memberCodeTTLSeconds})
}

type claimNewcomerRequest struct {
	Phone string `json:"phone"`
}

// ClaimNewcomer claims the 新人见面礼 coupon for the caller (idempotent: a member
// who already holds a newmember-sourced coupon gets that one back, no re-issue).
// POST /v1/wxshop/member/claim-newcomer  body{phone}
func (h *WxshopHandler) ClaimNewcomer(c *gin.Context) {
	var req claimNewcomerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	ctx := c.Request.Context()
	m := h.memberFromClaims(c, hashPhone(req.Phone))
	if m == nil {
		return
	}

	// Idempotency: already claimed a newcomer coupon → return the existing one.
	if existing, err := h.marketing.ListMemberCoupons(ctx, m.ID, ""); err == nil {
		for _, mc := range existing {
			if mc.Source == marketing.SourceNewmember {
				tpl, terr := h.marketing.GetCoupon(ctx, mc.CouponID)
				if terr != nil {
					tpl = nil
				}
				httpx.OK(c, gin.H{"coupon": toCouponDTO(mc, tpl, h.now()), "already_claimed": true})
				return
			}
		}
	}

	// Resolve (or auto-create) the tenant's 新人券 template.
	tpl, err := h.findOrCreateNewcomerCoupon(c)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	exp := h.now().Add(7 * 24 * time.Hour) // 领取后 7 天内有效
	mc, err := h.marketing.Issue(ctx, marketing.IssueInput{
		MemberID: m.ID, CouponID: tpl.ID, Source: marketing.SourceNewmember, ExpireAt: &exp,
	})
	if err != nil {
		h.failWxMarketing(c, err)
		return
	}
	httpx.OK(c, gin.H{"coupon": toCouponDTO(mc, tpl, h.now())})
}

// findOrCreateNewcomerCoupon returns an active 新人 coupon template for the
// caller's tenant, auto-creating a default 满30减10 (7-day) one when none exists
// so the demo flow always has a claimable newcomer gift.
func (h *WxshopHandler) findOrCreateNewcomerCoupon(c *gin.Context) (*marketing.Coupon, error) {
	ctx := c.Request.Context()
	list, err := h.marketing.ListCoupons(ctx, marketing.CouponFilter{Status: marketing.StatusActive, Limit: 100})
	if err != nil {
		return nil, err
	}
	for _, tpl := range list {
		if tpl != nil && containsNewcomer(tpl.Name) {
			return tpl, nil
		}
	}
	// None active: create a default newcomer template and activate it.
	now := h.now()
	created, err := h.marketing.CreateCoupon(ctx, marketing.CreateCouponInput{
		Name:       newcomerCouponName,
		Type:       marketing.TypeFullReduction,
		Params:     marketing.CouponParams{ThresholdCents: 3000, OffCents: 1000}, // 满30减10
		ValidFrom:  now,
		ValidTo:    now.Add(365 * 24 * time.Hour),
		TotalQuota: 0, // 不限量
	})
	if err != nil {
		return nil, err
	}
	return h.marketing.UpdateStatus(ctx, created.ID, marketing.StatusActive)
}

// containsNewcomer reports whether a coupon title marks it as a 新人券.
func containsNewcomer(name string) bool {
	for _, kw := range []string{"新人", "新客", "见面礼", "新会员"} {
		if strings.Contains(name, kw) {
			return true
		}
	}
	return false
}

// hashPhone produces the privacy-preserving phone hash stored on the member row.
// The consumer only ever submits a phone at enrolment; we never store the raw
// number. (Membership matches on wx_openid first, so a coarse FNV hash suffices
// as the Join idempotency phone key without persisting PII.)
func hashPhone(phone string) string {
	if phone == "" {
		return ""
	}
	const offset64 = 14695981039346656037
	const prime64 = 1099511628211
	h := uint64(offset64)
	for i := 0; i < len(phone); i++ {
		h ^= uint64(phone[i])
		h *= prime64
	}
	return fmt.Sprintf("ph%x", h)
}

// groupbuyDTO is the C-side 拼团简报, field-aligned with GroupBuyBrief.
type groupbuyDTO struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	PriceCents    int64  `json:"price_cents"`
	OriginalCents int64  `json:"original_cents"`
	Required      int    `json:"required"`
	Joined        int    `json:"joined"`
}

// GroupbuysActive lists the store's in-progress squads.
// GET /v1/wxshop/groupbuys/active
func (h *WxshopHandler) GroupbuysActive(c *gin.Context) {
	storeID := storeScope(c, 0) // ctx store; 0 = whole tenant
	list, err := h.groupbuy.ListActive(c.Request.Context(), storeID)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	out := make([]groupbuyDTO, 0, len(list))
	for _, g := range list {
		joined := g.Joined
		// Prefer the authoritative participant count when available.
		if parts, perr := h.groupbuy.ListParticipants(c.Request.Context(), g.ID); perr == nil {
			joined = len(parts)
		}
		out = append(out, groupbuyDTO{
			ID:         g.ID,
			Title:      g.Title,
			PriceCents: g.Params.PriceCents,
			// original_cents: 无原价字段,降级等于团购价(前端可据此渲染; TODO 补原价)
			OriginalCents: g.Params.PriceCents,
			Required:      g.RequiredCount,
			Joined:        joined,
		})
	}
	httpx.OK(c, out)
}

// GroupbuyJoin adds the caller to a squad.
// POST /v1/wxshop/groupbuys/:id/join
func (h *WxshopHandler) GroupbuyJoin(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	m := h.memberFromClaims(c, "")
	if m == nil {
		return
	}
	p, g, err := h.groupbuy.Join(c.Request.Context(), id, m.ID)
	if err != nil {
		h.failWxGroupbuy(c, err)
		return
	}
	httpx.OK(c, gin.H{"participant": p, "groupbuy": g})
}

// orderItemDTO is one C-side order line. Only consumer-visible fields are
// exposed: the snapshot name/unit, quantity and the unit price the member paid.
// CostCents (margin/归因) and ProductID (internal) are deliberately hidden.
type orderItemDTO struct {
	Name       string `json:"name"`
	Unit       string `json:"unit"`
	Qty        int64  `json:"qty"`
	PriceCents int64  `json:"price_cents"`
}

// orderDTO is the C-side 我的订单 row (CONTRACT §16.3(7)). It deliberately
// exposes ONLY what a consumer should see — NEVER CostCents, MemberID,
// TenantID, StoreID, OperatorID, DiscountCents or PaymentRef (those are
// internal/boss-only). Items collapse to their consumer-facing subset.
type orderDTO struct {
	ID         int64          `json:"id"`
	OrderNo    string         `json:"order_no"`
	Status     string         `json:"status"` // pending|paid|refunded|voided
	TotalCents int64          `json:"total_cents"`
	PaidCents  int64          `json:"paid_cents"`
	CreatedAt  time.Time      `json:"created_at"`
	Items      []orderItemDTO `json:"items"`
}

// toOrderDTO maps a billing.Order into the consumer-facing DTO, dropping every
// internal field. Items are always a non-nil slice so the JSON renders [].
func toOrderDTO(o *billing.Order) orderDTO {
	items := make([]orderItemDTO, 0, len(o.Items))
	for _, it := range o.Items {
		items = append(items, orderItemDTO{
			Name:       it.NameSnapshot,
			Unit:       it.Unit,
			Qty:        it.Qty,
			PriceCents: it.PriceCents,
		})
	}
	return orderDTO{
		ID:         o.ID,
		OrderNo:    o.OrderNo,
		Status:     o.Status,
		TotalCents: o.TotalCents,
		PaidCents:  o.PaidCents,
		CreatedAt:  o.CreatedAt,
		Items:      items,
	}
}

// Orders lists the caller's交易记录 (我的订单). The member is resolved idempotently
// from the verified claims (proof of identity + tenant); the orders are then
// scoped to that member within the ctx tenant via billing.ListByMember —
// walk-in (MemberID=0) and cross-tenant orders are never returned (ARCH-01).
// When billing is not wired (minimal fixture) the endpoint degrades to an empty
// list rather than 404 (CONTRACT §16.3(7)).
// GET /v1/wxshop/orders
func (h *WxshopHandler) Orders(c *gin.Context) {
	m := h.memberFromClaims(c, "")
	if m == nil {
		return
	}
	if h.billing == nil {
		// Billing not wired (minimal fixture without the billing stack): degrade
		// to an empty list, never a 404 (CONTRACT §16.3(7)/§16.8).
		httpx.OK(c, []any{})
		return
	}
	orders, err := h.billing.ListByMember(c.Request.Context(), m.ID)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	out := make([]orderDTO, 0, len(orders))
	for _, o := range orders {
		out = append(out, toOrderDTO(o))
	}
	httpx.OK(c, out)
}

// Balance returns the caller's stored-value (储值) balance, split into the real
// paid-in balance and the bonus/gift part (CONTRACT §16.8). The member is
// resolved idempotently from the verified claims; the account is read within
// the ctx tenant via membership.GetAccount. A member with no account yet (e.g.
// a brand-new enrolment whose account open was bypassed) gets 0/0 — never a
// 404 — because Join normally opens a zero-balance account.
// GET /v1/wxshop/member/balance
func (h *WxshopHandler) Balance(c *gin.Context) {
	m := h.memberFromClaims(c, "")
	if m == nil {
		return
	}
	acc, err := h.membership.GetAccount(c.Request.Context(), m.ID)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			// No account row → a new member: report zero balances, not 404.
			httpx.OK(c, gin.H{"balance_cents": int64(0), "gift_cents": int64(0)})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{
		"balance_cents": acc.BalanceCents,
		"gift_cents":    acc.GiftCents,
	})
}

// failWxMarketing maps marketing domain errors to HTTP codes (C 端口径).
func (h *WxshopHandler) failWxMarketing(c *gin.Context, err error) {
	switch {
	case errors.Is(err, marketing.ErrNotFound):
		httpx.Fail(c, errcode.Err{Code: 40403, Message: "券不存在", HTTP: 404})
	case errors.Is(err, marketing.ErrQuotaExhausted):
		httpx.Fail(c, errcode.ErrCouponQuota)
	case errors.Is(err, marketing.ErrCouponInactive):
		httpx.Fail(c, errcode.ErrCouponInactive)
	case errors.Is(err, marketing.ErrCouponExpired):
		httpx.Fail(c, errcode.ErrCouponExpired)
	case errors.Is(err, margin.ErrBelowMarginFloor):
		httpx.Fail(c, errcode.ErrMarginGuard)
	default:
		httpx.FailError(c, err)
	}
}

// failWxGroupbuy maps groupbuy domain errors to HTTP codes (C 端口径).
func (h *WxshopHandler) failWxGroupbuy(c *gin.Context, err error) {
	switch {
	case errors.Is(err, groupbuy.ErrNotFound):
		httpx.Fail(c, errcode.Err{Code: 40404, Message: "拼团不存在", HTTP: 404})
	case errors.Is(err, groupbuy.ErrGroupbuyFull):
		httpx.Fail(c, errcode.ErrGroupbuyFull)
	case errors.Is(err, groupbuy.ErrGroupbuyClosed):
		httpx.Fail(c, errcode.ErrGroupbuyClosed)
	case errors.Is(err, groupbuy.ErrAlreadyJoined):
		httpx.Fail(c, errcode.ErrAlreadyJoined)
	default:
		httpx.FailError(c, err)
	}
}
