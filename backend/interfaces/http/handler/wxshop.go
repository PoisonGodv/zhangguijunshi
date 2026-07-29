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
	"github.com/zhangguijunshi/backend/internal/store"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// WxshopHandler serves the consumer business endpoints.
type WxshopHandler struct {
	membership *membership.Service
	marketing  *marketing.Service
	groupbuy   *groupbuy.Service
	billing    *billing.Service // optional; nil → /orders degrades to empty list (minimal fixture)
	store      *store.Service   // optional; nil → /stores degrades to empty list
	now        func() time.Time
}

// NewWxshopHandler constructs a WxshopHandler. membership/marketing/groupbuy
// are required for the endpoints that are wired; billing/store may be nil.
func NewWxshopHandler(mem *membership.Service, mkt *marketing.Service, gb *groupbuy.Service, bil *billing.Service, st *store.Service) *WxshopHandler {
	return &WxshopHandler{membership: mem, marketing: mkt, groupbuy: gb, billing: bil, store: st, now: time.Now}
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

// Profile returns the caller's member profile. balance_cents reflects the real
// stored-value balance via membership.GetAccount (a member with no account yet
// reads 0, matching the Balance handler). phone_masked stays empty (the member
// only stores a hash, never the raw number).
// GET /v1/wxshop/member/profile
func (h *WxshopHandler) Profile(c *gin.Context) {
	m := h.memberFromClaims(c, "")
	if m == nil {
		return
	}
	// Real stored-value balance (CONTRACT §16.4: the GetAccount path is wired
	// in FE-MP-02 — the §16.4 TODO is retired). A member whose account open was
	// bypassed reads 0, matching the /member/balance handler.
	var balance int64
	if acc, err := h.membership.GetAccount(c.Request.Context(), m.ID); err == nil && acc != nil {
		balance = acc.BalanceCents
	}
	httpx.OK(c, gin.H{
		"member_id":         m.ID,
		"phone_masked":      "", // 手机号仅存 hash,无法回显; C 端档案不展示手机号
		"level":             m.Level,
		"level_name":        memberLevelName(m.Level),
		"points":            m.Points,
		"balance_cents":     balance,
		"total_spent_cents": m.TotalSpentCents,
		"tenant_id":         m.TenantID,
		"store_id":          m.StoreID,
	})
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

// groupbuyDetailDTO is the C-side 拼团详情, the extended shape of groupbuyDTO
// used by the detail endpoint. It adds the C 端-visible fields (status / 描述 /
// 参团人数 / 成团门槛 / 团购价/原价 / 截止时间) and hides every internal field
// (StrategyID / ProductID / TenantID / StoreID / Joined counter / timestamps
// beyond EndAt). The participant count comes from ListParticipants (authoritative).
type groupbuyDetailDTO struct {
	ID            int64     `json:"id"`
	Title         string    `json:"title"`
	Desc          string    `json:"desc"` // 描述(C 端文案; 本期=Title, TODO 补独立desc)
	PriceCents    int64     `json:"price_cents"`
	OriginalCents int64     `json:"original_cents"`
	Required      int       `json:"required"` // 成团门槛
	Joined        int       `json:"joined"`   // 权威参团人数(ListParticipants 计数)
	Status        string    `json:"status"`   // active|success|failed|cancelled
	Expire        time.Time `json:"expire"`   // EndAt, C 端倒计时用
}

// GroupbuyDetail returns the C-side 拼团详情 (CONTRACT §16.3(8)). It loads the
// squad via groupbuy.Get (cross-tenant → 40404, indistinguishable from
// absence) and the authoritative participant count via ListParticipants. The
// response hides every internal field; a caller without a verified member
// identity still GETs the detail (参团前浏览), so memberFromClaims is NOT
// required here — only the verified tenant from ctx (the groupbuy service
// EnforceTenant's the call). status maps the squad's terminal state.
// GET /v1/wxshop/groupbuys/:id
func (h *WxshopHandler) GroupbuyDetail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	g, err := h.groupbuy.Get(c.Request.Context(), id)
	if err != nil {
		h.failWxGroupbuy(c, err)
		return
	}
	joined := g.Joined
	if parts, perr := h.groupbuy.ListParticipants(c.Request.Context(), g.ID); perr == nil {
		joined = len(parts)
	}
	httpx.OK(c, groupbuyDetailDTO{
		ID:            g.ID,
		Title:         g.Title,
		Desc:          g.Title, // 本期 desc = title (TODO 独立 desc 字段)
		PriceCents:    g.Params.PriceCents,
		OriginalCents: g.Params.PriceCents, // 无原价字段,降级=团购价 (CONTRACT §16.4 TODO)
		Required:      g.RequiredCount,
		Joined:        joined,
		Status:        g.Status,
		Expire:        g.EndAt,
	})
}

// couponDetailDTO extends couponDTO with the C-side 券详情 fields: the usage
// rule text (threshold / scope / value) and the validity window. The member's
// holding status + expire are merged in when the caller holds the coupon;
// otherwise status defaults to "available" (the template is claimable).
type couponDetailDTO struct {
	couponDTO
	Detail    couponDetail `json:"detail"`
	Available bool         `json:"available"` // true = caller may still claim/use
	Expire    string       `json:"expire"`    // ISO date or "" when none
}

// couponDetail is the C-side 券详情: the human-readable usage rule + validity.
type couponDetail struct {
	ThresholdText string `json:"threshold_text,omitempty"` // 满¥N可用 / 无门槛
	ScopeText     string `json:"scope_text,omitempty"`     // 全场通用 / 指定商品
	ValueText     string `json:"value_text,omitempty"`     // ¥N / N折 / 赠品
	ValidFrom     string `json:"valid_from,omitempty"`     // ISO date
	ValidTo       string `json:"valid_to,omitempty"`       // ISO date
}

// CouponDetail returns the C-side 券详情 (CONTRACT §16.3(9)). The template is
// loaded via marketing.GetCoupon (cross-tenant → 40403, indistinguishable from
// absence). The caller's member row is resolved via memberFromClaims (idempotent
// Join) so the holding status + expire can be merged: if the member holds this
// coupon (ListMemberCoupons matches CouponID) the response carries its real
// status + expire; otherwise status="available" and available=true (the
// template is claimable). Internal fields (quota / issued / strategy_id) are
// hidden.
// GET /v1/wxshop/coupons/:id
func (h *WxshopHandler) CouponDetail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	tpl, err := h.marketing.GetCoupon(c.Request.Context(), id)
	if err != nil {
		h.failWxMarketing(c, err)
		return
	}
	// The caller's member identity (idempotent Join) — merged in best-effort:
	// a caller with no verified identity still GETs the template (available=true).
	var memberID int64
	if m := h.memberFromClaims(c, ""); m != nil {
		memberID = m.ID
	}
	now := h.now()
	detail := couponDetail{
		ValueText: templateValueText(tpl),
		ValidFrom: tpl.ValidFrom.Local().Format("2006-01-02"),
		ValidTo:   tpl.ValidTo.Local().Format("2006-01-02"),
	}
	switch tpl.Type {
	case marketing.TypeDiscount:
		detail.ThresholdText = "无门槛"
		detail.ScopeText = "全场通用"
	case marketing.TypeGift:
		detail.ScopeText = "指定商品"
	default: // full_reduction | cash
		if tpl.Params.ThresholdCents > 0 {
			detail.ThresholdText = "满" + formatYuan(tpl.Params.ThresholdCents) + "可用"
		} else {
			detail.ThresholdText = "无门槛"
		}
		detail.ScopeText = "全场通用"
	}

	out := couponDetailDTO{
		couponDTO: couponDTO{
			ID:         tpl.ID,
			Title:      tpl.Name,
			Type:       couponType(tpl),
			ValueText:  templateValueText(tpl),
			ExpireText: tpl.ValidTo.Local().Format("2006-01-02") + " 到期",
			Status:     "available",
		},
		Detail:    detail,
		Available: tpl.Status == marketing.StatusActive && now.Before(tpl.ValidTo),
		Expire:    tpl.ValidTo.Local().Format("2006-01-02"),
	}

	// Merge the member's holding status + expire when they hold this coupon.
	if memberID > 0 {
		if list, lerr := h.marketing.ListMemberCoupons(c.Request.Context(), memberID, ""); lerr == nil {
			for _, mc := range list {
				if mc.CouponID == tpl.ID {
					out.couponDTO.Status = toCouponDTO(mc, tpl, now).Status
					out.Available = false // they already hold it
					if mc.ExpireAt != nil {
						out.Expire = mc.ExpireAt.Local().Format("2006-01-02")
					}
					break
				}
			}
		}
	}
	httpx.OK(c, out)
}

// couponType maps a coupon template to the C-side type set (cash|discount|gift).
func couponType(tpl *marketing.Coupon) string {
	switch tpl.Type {
	case marketing.TypeDiscount:
		return "discount"
	case marketing.TypeGift:
		return "gift"
	default: // full_reduction | cash → "cash"
		return "cash"
	}
}

// templateValueText renders the value text for a coupon template (¥N / N折 /
// 赠品) — the same shape toCouponDTO produces, factored so the detail endpoint
// can render a template the caller does not yet hold.
func templateValueText(tpl *marketing.Coupon) string {
	switch tpl.Type {
	case marketing.TypeDiscount:
		return formatDiscount(tpl.Params.DiscountPermille)
	case marketing.TypeGift:
		return "赠品"
	default: // full_reduction | cash
		return formatYuan(tpl.Params.OffCents)
	}
}

// pointsLogDTO is the C-side 积分明细 row, the snake_case shape of
// membership.PointsLog. ref_order_id stays (a member may recognise their
// order); the internal id / tenant_id / member_id are dropped (C 端 never
// needs them).
type pointsLogDTO struct {
	Type        string `json:"type"` // earn | redeem | expire | adjust
	Delta       int64  `json:"delta"`
	PointsAfter int64  `json:"points_after"`
	RefOrderID  int64  `json:"ref_order_id,omitempty"`
	CreatedAt   string `json:"created_at"` // ISO 8601
}

// Points returns the caller's 积分明细 (CONTRACT §16.3(10)). The member is
// resolved idempotently from the verified claims; the points log is read within
// the ctx tenant via membership.ListPointsLog. The current points balance is
// the member row's Points (the authoritative running balance the logs sum to).
// limit defaults to 50; ?limit= caps it. Cross-tenant reads return an empty
// logs slice + 0 points (the member row is tenant-scoped via Join).
// GET /v1/wxshop/member/points
func (h *WxshopHandler) Points(c *gin.Context) {
	m := h.memberFromClaims(c, "")
	if m == nil {
		return
	}
	limit := 50
	if q := c.Query("limit"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 && v <= 200 {
			limit = v
		}
	}
	logs, err := h.membership.ListPointsLog(c.Request.Context(), m.ID, limit)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	out := make([]pointsLogDTO, 0, len(logs))
	for _, l := range logs {
		out = append(out, pointsLogDTO{
			Type:        l.Type,
			Delta:       l.Delta,
			PointsAfter: l.PointsAfter,
			RefOrderID:  l.RefOrderID,
			CreatedAt:   l.CreatedAt.Local().Format(time.RFC3339),
		})
	}
	httpx.OK(c, gin.H{
		"points": m.Points,
		"logs":   out,
	})
}

// storeDTO is the C-side 门店 row, the snake_case shape of store.Store. Only
// the consumer-facing fields surface; the internal tenant_id / created_at /
// active flag are dropped (the list already filters active + tenant-scoped).
type storeDTO struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Phone   string `json:"phone"`
	GroupQR string `json:"group_qr,omitempty"`
}

// Stores returns the C-side 门店列表 (CONTRACT §16.3(11)). The tenant is
// resolved from the verified ctx (EnforceTenant in the store service); a
// caller without a verified tenant gets 40302 (EnforceTenant). When the store
// service is not wired (minimal fixture) the endpoint degrades to an empty
// list rather than 404. Cross-tenant stores are never visible (empty list).
// GET /v1/wxshop/stores
func (h *WxshopHandler) Stores(c *gin.Context) {
	if h.store == nil {
		httpx.OK(c, []any{})
		return
	}
	list, err := h.store.ListStores(c.Request.Context())
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	out := make([]storeDTO, 0, len(list))
	for _, s := range list {
		out = append(out, storeDTO{
			ID:      s.ID,
			Name:    s.Name,
			Address: s.Address,
			Phone:   s.Phone,
			GroupQR: s.GroupQR,
		})
	}
	httpx.OK(c, out)
}
