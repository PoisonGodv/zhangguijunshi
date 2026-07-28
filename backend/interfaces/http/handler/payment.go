package handler

// PaymentHandler exposes the boss-surface WeChat-pay service-provider endpoints
// (M4-S1, CONTRACT §15.7): sub-merchant onboarding, unified-order (prepay),
// payment lookup, the audit ledger, and the unauthenticated notify callback.
// Tenant/store come from the verified request context for all authed routes;
// the notify route recovers the tenant from the out_trade_no (it has no auth).

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/payment"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// PaymentHandler serves payment endpoints.
type PaymentHandler struct {
	svc *payment.Service
}

// NewPaymentHandler constructs a PaymentHandler.
func NewPaymentHandler(svc *payment.Service) *PaymentHandler {
	return &PaymentHandler{svc: svc}
}

// errPaymentNotFound is the boss-side not-found for sub-merchant / payment rows
// (cross-tenant reads are indistinguishable from absence, ARCH-01).
var errPaymentNotFound = errcode.Err{Code: 40403, Message: "记录不存在", HTTP: 404}

type applySubMerchantRequest struct {
	StoreID      int64  `json:"store_id"`
	Provider     string `json:"provider"`
	BusinessName string `json:"business_name" binding:"required"`
	LicenseNo    string `json:"license_no" binding:"required"`
	LicenseType  string `json:"license_type"`
	ContactName  string `json:"contact_name"`
	ContactPhone string `json:"contact_phone"`
	RateBPS      int32  `json:"rate_bps"`
}

// ApplySubMerchant submits a sub-merchant onboarding application.
// POST /v1/boss/payment/sub-merchants
func (h *PaymentHandler) ApplySubMerchant(c *gin.Context) {
	var req applySubMerchantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	provider := req.Provider
	if provider == "" {
		provider = payment.ProviderWxPay
	}
	licenseType := req.LicenseType
	if licenseType == "" {
		licenseType = "individual"
	}
	sm, err := h.svc.ApplySubMerchant(c.Request.Context(), payment.ApplySubMerchantInput{
		StoreID:      storeScope(c, req.StoreID),
		Provider:     provider,
		BusinessName: req.BusinessName,
		LicenseNo:    req.LicenseNo,
		LicenseType:  licenseType,
		ContactName:  req.ContactName,
		ContactPhone: req.ContactPhone,
		RateBPS:      req.RateBPS,
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, sm)
}

// ListSubMerchants lists the caller tenant's sub-merchants.
// GET /v1/boss/payment/sub-merchants
func (h *PaymentHandler) ListSubMerchants(c *gin.Context) {
	list, err := h.svc.ListSubMerchants(c.Request.Context())
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, list)
}

// QuerySubMerchant polls the onboarding status and writes it through.
// POST /v1/boss/payment/sub-merchants/:id/query
func (h *PaymentHandler) QuerySubMerchant(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	sm, err := h.svc.QuerySubMerchant(c.Request.Context(), id)
	if err != nil {
		if err == payment.ErrNotFound {
			httpx.Fail(c, errPaymentNotFound)
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, sm)
}

type prepayRequest struct {
	StoreID     int64  `json:"store_id"`
	OutTradeNo  string `json:"out_trade_no"` // optional; empty = server-generated
	Subject     string `json:"subject" binding:"required"`
	RefID       int64  `json:"ref_id"`
	AmountCents int64  `json:"amount_cents" binding:"required,min=1"`
	OpenID      string `json:"openid"`
	Description string `json:"description"`
}

// Prepay creates a pending payment and calls unified-order. Requires an active
// sub-merchant for the store (反二清: there must be a clearer).
// POST /v1/boss/payment/prepay
func (h *PaymentHandler) Prepay(c *gin.Context) {
	var req prepayRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	p, result, err := h.svc.Prepay(c.Request.Context(), payment.PrepayInput{
		StoreID:     storeScope(c, req.StoreID),
		OutTradeNo:  req.OutTradeNo,
		Subject:     req.Subject,
		RefID:       req.RefID,
		AmountCents: req.AmountCents,
		OpenID:      req.OpenID,
		Description: req.Description,
	})
	if err != nil {
		switch err {
		case payment.ErrSubMerchantNotActive:
			httpx.Fail(c, errcode.ErrSubMerchantNotActive)
		case payment.ErrDuplicateOutTradeNo:
			httpx.Fail(c, errcode.ErrPaymentDuplicate)
		default:
			httpx.FailError(c, err)
		}
		return
	}
	httpx.OK(c, gin.H{"payment": p, "prepay": result})
}

// GetPayment loads a payment by id within the caller tenant.
// GET /v1/boss/payment/payments/:id
func (h *PaymentHandler) GetPayment(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	p, err := h.svc.GetPayment(c.Request.Context(), id)
	if err != nil {
		if err == payment.ErrNotFound {
			httpx.Fail(c, errPaymentNotFound)
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, p)
}

// ListLedger returns the caller tenant's audit ledger rows (newest first).
// GET /v1/boss/payment/ledger?limit=
func (h *PaymentHandler) ListLedger(c *gin.Context) {
	limit := int(queryInt64(c, "limit"))
	rows, err := h.svc.ListLedger(c.Request.Context(), limit)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, rows)
}

// Notify is the unauthenticated WeChat-pay callback entry (CONTRACT §15.7).
// POST /v1/wxshop/payment/notify — no auth/tenant middleware. The tenant is
// recovered from the out_trade_no in the body, injected into ctx, then
// HandleNotify runs the idempotent pending→success + fulfil pipeline. Replies
// with WeChat's required {code,message} JSON; FAIL tells WeChat to retry.
func (h *PaymentHandler) Notify(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		c.JSON(200, gin.H{"code": "FAIL", "message": "read body fail"})
		return
	}
	ctx := c.Request.Context()
	tenantID, err := h.svc.ResolveTenantByNotifyBody(ctx, body)
	if err != nil {
		c.JSON(200, gin.H{"code": "FAIL", "message": "resolve tenant fail"})
		return
	}
	tctx := multitenancy.WithContext(ctx, multitenancy.Context{
		TenantID: tenantID,
		Source:   multitenancy.SourceSystem,
	})
	if _, err := h.svc.HandleNotify(tctx, nil, body); err != nil {
		c.JSON(200, gin.H{"code": "FAIL", "message": "handle fail"})
		return
	}
	c.JSON(200, gin.H{"code": "SUCCESS", "message": "成功"})
}
