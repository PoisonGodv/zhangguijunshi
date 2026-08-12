package handler

// CatalogHandler exposes the boss-surface product catalogue endpoints
// (M1-S1, CONTRACT §8). Tenant/store are resolved from the verified request
// context (JWT in prod, X-Mock-* in dev) — never from the request body.

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// CatalogHandler serves catalogue endpoints.
type CatalogHandler struct {
	svc *catalog.Service
}

// NewCatalogHandler constructs a CatalogHandler.
func NewCatalogHandler(svc *catalog.Service) *CatalogHandler {
	return &CatalogHandler{svc: svc}
}

type createProductRequest struct {
	Barcode       string   `json:"barcode"`
	Name          string   `json:"name" binding:"required"`
	Category      string   `json:"category"`
	Unit          string   `json:"unit"`
	CostCents     int64    `json:"cost_cents"`
	PriceCents    int64    `json:"price_cents"`
	ImageURL      string   `json:"image_url"`
	ShelfLifeDays int      `json:"shelf_life_days"`
	Tags          []string `json:"tags"`
}

// CreateProduct creates a product under the caller's tenant+store.
// POST /v1/boss/catalog/products
func (h *CatalogHandler) CreateProduct(c *gin.Context) {
	var req createProductRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	p, err := h.svc.Create(c.Request.Context(), catalog.CreateInput{
		Barcode:       req.Barcode,
		Name:          req.Name,
		Category:      req.Category,
		Unit:          req.Unit,
		CostCents:     req.CostCents,
		PriceCents:    req.PriceCents,
		ImageURL:      req.ImageURL,
		ShelfLifeDays: req.ShelfLifeDays,
		Tags:          req.Tags,
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, p)
}

// ListProducts lists the tenant's products with optional filters and returns
// the FULL match count for true pagination (CONTRACT §24.5): {items, total}.
// Pagination params accept both ?limit=&offset= and ?page=&size= (same
// dual-param idiom as the membership list handler).
// GET /v1/boss/catalog/products?store_id=&query=&category=&limit=&offset=
func (h *CatalogHandler) ListProducts(c *gin.Context) {
	f := catalog.ProductFilter{
		StoreID:  queryInt64(c, "store_id"),
		Query:    c.Query("query"),
		Category: c.Query("category"),
		Limit:    int(queryInt64(c, "limit")),
		Offset:   int(queryInt64(c, "offset")),
	}
	// ?page=&size= alias (membership handler parity): page>=1, size<=200 cap;
	// wins only when limit/offset are absent so explicit limit/offset still works.
	if f.Limit <= 0 && f.Offset <= 0 {
		if size := int(queryInt64(c, "size")); size > 0 {
			page := int(queryInt64(c, "page"))
			if page < 1 {
				page = 1
			}
			if size > 200 {
				size = 200
			}
			f.Limit = size
			f.Offset = (page - 1) * size
		}
	}
	res, err := h.svc.ListPage(c.Request.Context(), f)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"items": res.Items, "total": res.Total})
}

// GetProduct loads one product by id within the caller's tenant.
// GET /v1/boss/catalog/products/:id
func (h *CatalogHandler) GetProduct(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	p, err := h.svc.GetByID(c.Request.Context(), id)
	if err != nil {
		failNotFound(c, err)
		return
	}
	httpx.OK(c, p)
}

// GetProductByBarcode loads a product by barcode within the caller's store.
// GET /v1/boss/catalog/products/barcode/:barcode
func (h *CatalogHandler) GetProductByBarcode(c *gin.Context) {
	barcode := c.Param("barcode")
	if barcode == "" {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	p, err := h.svc.GetByBarcode(c.Request.Context(), barcode)
	if err != nil {
		failNotFound(c, err)
		return
	}
	httpx.OK(c, p)
}

// importProductsRequest is the batch-import body (CONTRACT §25.6). Items reuse
// the single-create DTO so the per-row contract is identical to CreateProduct.
type importProductsRequest struct {
	Items []createProductRequest `json:"items"`
}

// importMaxRows caps a single import batch (abuse/latency guard, CONTRACT §25.6).
const importMaxRows = 500

// ImportProducts bulk-creates products from a JSON array. Rows are processed
// one by one (best-effort): a bad/duplicate row is reported in errors with its
// 1-based row index and never aborts the batch. Empty items or >500 rows -> 400.
// POST /v1/boss/catalog/products/import
func (h *CatalogHandler) ImportProducts(c *gin.Context) {
	var req importProductsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	if len(req.Items) == 0 || len(req.Items) > importMaxRows {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	items := make([]catalog.CreateInput, 0, len(req.Items))
	for _, it := range req.Items {
		items = append(items, catalog.CreateInput{
			Barcode:       it.Barcode,
			Name:          it.Name,
			Category:      it.Category,
			Unit:          it.Unit,
			CostCents:     it.CostCents,
			PriceCents:    it.PriceCents,
			ImageURL:      it.ImageURL,
			ShelfLifeDays: it.ShelfLifeDays,
			Tags:          it.Tags,
		})
	}
	res := h.svc.ImportBatch(c.Request.Context(), items)
	httpx.OK(c, gin.H{
		"imported": res.Imported,
		"failed":   res.Failed,
		"errors":   res.Errors,
	})
}

type updatePriceRequest struct {
	CostCents  int64 `json:"cost_cents"`
	PriceCents int64 `json:"price_cents"`
}

// UpdateProductPrice re-prices a product (price >= cost >= 0).
// PATCH /v1/boss/catalog/products/:id/price
func (h *CatalogHandler) UpdateProductPrice(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req updatePriceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	p, err := h.svc.UpdatePrice(c.Request.Context(), id, req.CostCents, req.PriceCents)
	if err != nil {
		failNotFound(c, err)
		return
	}
	httpx.OK(c, p)
}

// failNotFound maps a domain "not found" to a 404 envelope, forwarding every
// other error to the standard mapper.
func failNotFound(c *gin.Context, err error) {
	if err == catalog.ErrNotFound {
		httpx.Fail(c, errcode.Err{Code: 40401, Message: "资源不存在", HTTP: 404})
		return
	}
	httpx.FailError(c, err)
}

// queryInt64 reads an int64 query param, returning 0 when absent/invalid.
func queryInt64(c *gin.Context, key string) int64 {
	v, _ := strconv.ParseInt(c.Query(key), 10, 64)
	return v
}

// storeScope resolves the effective store id for order/summary endpoints: the
// explicit param wins, else the store carried in the verified context.
func storeScope(c *gin.Context, explicit int64) int64 {
	if explicit > 0 {
		return explicit
	}
	if mc, ok := multitenancy.FromContext(c.Request.Context()); ok {
		return mc.StoreID
	}
	return 0
}
