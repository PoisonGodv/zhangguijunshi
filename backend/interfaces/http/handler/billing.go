package handler

// BillingHandler exposes the boss-surface checkout + daily-flow endpoints
// (M1-S1, CONTRACT §8). Tenant/store come from the verified request context.

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// BillingHandler serves billing endpoints.
type BillingHandler struct {
	svc *billing.Service
}

// NewBillingHandler constructs a BillingHandler.
func NewBillingHandler(svc *billing.Service) *BillingHandler {
	return &BillingHandler{svc: svc}
}

type createOrderItemRequest struct {
	ProductID int64 `json:"product_id" binding:"required,min=1"`
	Qty       int64 `json:"qty" binding:"required,min=1"`
}

type createOrderRequest struct {
	StoreID       int64                    `json:"store_id"`
	MemberID      int64                    `json:"member_id"`
	Channel       string                   `json:"channel"`
	PaymentMethod string                   `json:"payment_method"`
	Items         []createOrderItemRequest `json:"items" binding:"required,min=1,dive"`
}

// CreateOrder prices and records a paid order under the caller's tenant.
// POST /v1/boss/billing/orders
func (h *BillingHandler) CreateOrder(c *gin.Context) {
	var req createOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	items := make([]billing.CreateOrderItem, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, billing.CreateOrderItem{ProductID: it.ProductID, Qty: it.Qty})
	}
	order, err := h.svc.CreateOrder(c.Request.Context(), billing.CreateOrderInput{
		StoreID:       req.StoreID,
		MemberID:      req.MemberID,
		Channel:       req.Channel,
		PaymentMethod: req.PaymentMethod,
		Items:         items,
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, order)
}

// GetOrder loads one order (with items) within the caller's tenant.
// GET /v1/boss/billing/orders/:id
func (h *BillingHandler) GetOrder(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	order, err := h.svc.GetOrder(c.Request.Context(), id)
	if err != nil {
		if err == billing.ErrNotFound {
			httpx.Fail(c, errcode.Err{Code: 40401, Message: "订单不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, order)
}

// DailySummaryToday returns the caller's flow summary for today.
// GET /v1/boss/billing/summary/today?store_id=
func (h *BillingHandler) DailySummaryToday(c *gin.Context) {
	storeID := storeScope(c, queryInt64(c, "store_id"))
	sum, err := h.svc.DailySummary(c.Request.Context(), storeID, time.Now())
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, sum)
}

// DailySummary returns the caller's flow summary for a specific date.
// GET /v1/boss/billing/summary/:date?store_id=   (date = YYYY-MM-DD)
func (h *BillingHandler) DailySummary(c *gin.Context) {
	day, err := time.ParseInLocation("2006-01-02", c.Param("date"), time.Local)
	if err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	storeID := storeScope(c, queryInt64(c, "store_id"))
	sum, err := h.svc.DailySummary(c.Request.Context(), storeID, day)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, sum)
}
