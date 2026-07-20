package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// testPricer adapts the catalog service to billing.ProductPricer for the HTTP
// fixture (mirrors the adapter in cmd/api/main.go).
type testPricer struct{ svc *catalog.Service }

func (p testPricer) GetByID(ctx context.Context, tenantID, id int64) (*billing.PricedProduct, error) {
	prod, err := p.svc.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if prod.TenantID != tenantID {
		return nil, catalog.ErrNotFound
	}
	return &billing.PricedProduct{
		ID: prod.ID, TenantID: prod.TenantID, StoreID: prod.StoreID,
		Name: prod.Name, Unit: prod.Unit,
		PriceCents: prod.PriceCents, CostCents: prod.CostCents, Status: prod.Status,
	}, nil
}

// newM1S1Router builds a router with the catalog+billing services wired over
// in-memory repos and the dev MockTenant header enabled (DevMock).
func newM1S1Router() *gin.Engine {
	catSvc := catalog.NewService(catalog.NewMemoryProductRepo())
	bilSvc := billing.New(billing.NewMemoryOrderRepo(), testPricer{catSvc})
	return New(Deps{
		Cfg:     &config.Config{App: config.AppConfig{Env: "test"}},
		Catalog: catSvc,
		Billing: bilSvc,
		DevMock: true,
	})
}

func doM1(t *testing.T, r *gin.Engine, method, path, tenant, store string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if tenant != "" {
		req.Header.Set("X-Mock-Tenant", tenant)
	}
	if store != "" {
		req.Header.Set("X-Mock-Store", store)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	env := decodeEnvelope(t, w.Body.Bytes())
	data, _ := env.Data.(map[string]any)
	return w, data
}

// TestCatalog_CreateAndGet proves the create→get round trip over HTTP with the
// tenant resolved from the mock header.
func TestCatalog_CreateAndGet(t *testing.T) {
	r := newM1S1Router()
	w, data := doM1(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "苹果", "unit": "斤", "cost_cents": 300, "price_cents": 500, "barcode": "6900001"})
	require.Equal(t, http.StatusOK, w.Code)
	assert.EqualValues(t, 50021, data["TenantID"])
	assert.EqualValues(t, 9, data["StoreID"])
	id := int64(data["ID"].(float64))
	require.Greater(t, id, int64(0))

	w2, got := doM1(t, r, http.MethodGet, fmt.Sprintf("/v1/boss/catalog/products/%d", id), "50021", "9", nil)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, "苹果", got["Name"])
}

// TestCatalog_CrossTenantHidden proves a product created by tenant A is
// invisible to tenant B through the same HTTP surface.
func TestCatalog_CrossTenantHidden(t *testing.T) {
	r := newM1S1Router()
	_, data := doM1(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "苹果", "unit": "斤", "cost_cents": 300, "price_cents": 500})
	id := int64(data["ID"].(float64))

	w, _ := doM1(t, r, http.MethodGet, fmt.Sprintf("/v1/boss/catalog/products/%d", id), "9999", "9", nil)
	assert.Equal(t, http.StatusNotFound, w.Code, "cross-tenant product read must 404")
}

// TestBilling_OrderAndSummary proves the order→daily-summary flow attributes
// gross/cost/profit correctly over HTTP.
func TestBilling_OrderAndSummary(t *testing.T) {
	r := newM1S1Router()
	_, prod := doM1(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "苹果", "unit": "斤", "cost_cents": 300, "price_cents": 500})
	pid := int64(prod["ID"].(float64))

	w, order := doM1(t, r, http.MethodPost, "/v1/boss/billing/orders", "50021", "9",
		gin.H{"store_id": 9, "items": []gin.H{{"product_id": pid, "qty": 2}}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.EqualValues(t, 1000, order["TotalCents"]) // 2 * 500
	assert.Equal(t, "paid", order["Status"])

	w2, sum := doM1(t, r, http.MethodGet, "/v1/boss/billing/summary/today?store_id=9", "50021", "9", nil)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.EqualValues(t, 1, sum["OrderCount"])
	assert.EqualValues(t, 1000, sum["GrossCents"])
	assert.EqualValues(t, 1000, sum["NetCents"])
	assert.EqualValues(t, 600, sum["CostCents"])   // 2 * 300
	assert.EqualValues(t, 400, sum["ProfitCents"]) // 1000 - 600
}

// TestBilling_CrossTenantOrderRejected proves tenant B cannot open an order
// against tenant A's product through the HTTP surface.
func TestBilling_CrossTenantOrderRejected(t *testing.T) {
	r := newM1S1Router()
	_, prod := doM1(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "苹果", "unit": "斤", "cost_cents": 300, "price_cents": 500})
	pid := int64(prod["ID"].(float64))

	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/billing/orders", "9999", "9",
		gin.H{"store_id": 9, "items": []gin.H{{"product_id": pid, "qty": 1}}})
	assert.NotEqual(t, http.StatusOK, w.Code, "cross-tenant order must be rejected")
}

// TestBilling_RejectsWithoutTenant proves the billing surface enforces the
// tenant guard: no tenant header → rejection (never a successful order).
func TestBilling_RejectsWithoutTenant(t *testing.T) {
	r := newM1S1Router()
	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/billing/orders", "", "",
		gin.H{"items": []gin.H{{"product_id": 1, "qty": 1}}})
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"no-tenant request must be rejected by the guard (ErrNoTenant -> 50001)")
}
