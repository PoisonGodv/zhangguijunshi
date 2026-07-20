package handler

// InventoryHandler exposes the boss-surface stock endpoints (M1-S2,
// CONTRACT §9.3 BE-A-04): purchase-in, current stock, and the movement ledger.
// Tenant/store come from the verified request context, never the request body.

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// InventoryHandler serves inventory endpoints.
type InventoryHandler struct {
	svc *inventory.Service
}

// NewInventoryHandler constructs an InventoryHandler.
func NewInventoryHandler(svc *inventory.Service) *InventoryHandler {
	return &InventoryHandler{svc: svc}
}

type purchaseInRequest struct {
	StoreID      int64  `json:"store_id"`
	ProductID    int64  `json:"product_id" binding:"required,min=1"`
	Quantity     int64  `json:"quantity" binding:"required,min=1"`
	InPriceCents int64  `json:"in_price_cents"`
	ExpiryDate   string `json:"expiry_date"` // YYYY-MM-DD, optional
	Note         string `json:"note"`
}

// PurchaseIn records an inbound batch for the caller's tenant.
// POST /v1/boss/inventory/purchase
func (h *InventoryHandler) PurchaseIn(c *gin.Context) {
	var req purchaseInRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var expiry *time.Time
	if req.ExpiryDate != "" {
		t, err := time.ParseInLocation("2006-01-02", req.ExpiryDate, time.Local)
		if err != nil {
			httpx.Fail(c, errcode.ErrParam)
			return
		}
		expiry = &t
	}
	batch, err := h.svc.PurchaseIn(c.Request.Context(), inventory.PurchaseInInput{
		StoreID:      storeScope(c, req.StoreID),
		ProductID:    req.ProductID,
		Quantity:     req.Quantity,
		InPriceCents: req.InPriceCents,
		ExpiryDate:   expiry,
		Note:         req.Note,
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, batch)
}

// GetStock returns the current active stock for a product.
// GET /v1/boss/inventory/stock?product_id=&store_id=
func (h *InventoryHandler) GetStock(c *gin.Context) {
	productID := queryInt64(c, "product_id")
	if productID <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	storeID := storeScope(c, queryInt64(c, "store_id"))
	qty, err := h.svc.Stock(c.Request.Context(), storeID, productID)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"product_id": productID, "store_id": storeID, "quantity": qty})
}

// ListMovements returns the stock movement ledger for the caller's tenant.
// GET /v1/boss/inventory/movements?store_id=&product_id=&type=&date=&limit=&offset=
func (h *InventoryHandler) ListMovements(c *gin.Context) {
	f := inventory.MovementFilter{
		StoreID:   storeScope(c, queryInt64(c, "store_id")),
		ProductID: queryInt64(c, "product_id"),
		Type:      c.Query("type"),
		Limit:     int(queryInt64(c, "limit")),
		Offset:    int(queryInt64(c, "offset")),
	}
	if d := c.Query("date"); d != "" {
		t, err := time.ParseInLocation("2006-01-02", d, time.Local)
		if err != nil {
			httpx.Fail(c, errcode.ErrParam)
			return
		}
		f.Day = &t
	}
	moves, err := h.svc.ListMovements(c.Request.Context(), f)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, moves)
}

// consumeRequest drives a manual FIFO deduction (rare; mainly for testing the
// stock path outside checkout). Checkout itself consumes via the StockConsumer.
type consumeRequest struct {
	StoreID  int64 `json:"store_id"`
	Qty      int64 `json:"qty" binding:"required,min=1"`
	RefOrder int64 `json:"ref_order_id"`
}

// Consume deducts stock FIFO for a product (used for adjustments/tests).
// POST /v1/boss/inventory/products/:id/consume
func (h *InventoryHandler) Consume(c *gin.Context) {
	productID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || productID <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req consumeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	batches, err := h.svc.ConsumeFIFO(c.Request.Context(), storeScope(c, req.StoreID), productID, req.Qty, req.RefOrder)
	if err != nil {
		if err == inventory.ErrInsufficientStock {
			httpx.Fail(c, errcode.Err{Code: 40002, Message: "库存不足", HTTP: 400})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, batches)
}
