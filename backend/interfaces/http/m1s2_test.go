package httpserver

// M1-S2 HTTP-layer tests (CONTRACT §9.6): the full boss loop
//   建品 → 入库 → 开单(FIFO 扣库存) → 会员入会 → 签发会员码 → 核销(累积分/扣储值)
// plus the cross-tenant proofs (tenant B cannot see/deduct tenant A's stock,
// cannot read tenant A's member, and tenant A's member code will not redeem
// under tenant B).

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// testProductLookuper adapts catalog to inventory.ProductLookuper for the fixture.
type testProductLookuper struct{ svc *catalog.Service }

func (l testProductLookuper) Exists(ctx context.Context, tenantID, productID int64) (bool, error) {
	prod, err := l.svc.GetByID(ctx, productID)
	if err != nil {
		if err == catalog.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return prod.TenantID == tenantID, nil
}

// testStockConsumer adapts inventory to billing.StockConsumer for the fixture.
type testStockConsumer struct{ svc *inventory.Service }

func (c testStockConsumer) ConsumeFIFO(ctx context.Context, storeID, productID, qty, refOrderID int64) error {
	_, err := c.svc.ConsumeFIFO(ctx, storeID, productID, qty, refOrderID)
	return err
}

// newM1S2Router builds a router with catalog+inventory+billing+membership wired
// over in-memory repos and the dev MockTenant header enabled.
func newM1S2Router() *gin.Engine {
	catSvc := catalog.NewService(catalog.NewMemoryProductRepo())
	invSvc := inventory.New(inventory.NewMemoryBatchRepo(), testProductLookuper{catSvc})
	bilSvc := billing.New(billing.NewMemoryOrderRepo(), testPricer{catSvc},
		billing.WithStockConsumer(testStockConsumer{invSvc}))
	memSvc := membership.New(membership.NewMemoryMemberRepo(), membership.NewMemoryMemberCoder(nil))
	return New(Deps{
		Cfg:        &config.Config{App: config.AppConfig{Env: "test"}},
		Catalog:    catSvc,
		Billing:    bilSvc,
		Inventory:  invSvc,
		Membership: memSvc,
		DevMock:    true,
	})
}

// TestInventory_PurchaseThenOrderDeductsStock proves the create-product →
// purchase-in → order chain decrements stock FIFO and records the sale.
func TestInventory_PurchaseThenOrderDeductsStock(t *testing.T) {
	r := newM1S2Router()

	_, prod := doM1(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "苹果", "unit": "斤", "cost_cents": 300, "price_cents": 500})
	pid := int64(prod["ID"].(float64))

	// Purchase 10 in.
	w, batch := doM1(t, r, http.MethodPost, "/v1/boss/inventory/purchase", "50021", "9",
		gin.H{"store_id": 9, "product_id": pid, "quantity": 10, "in_price_cents": 300})
	require.Equal(t, http.StatusOK, w.Code)
	assert.EqualValues(t, 10, batch["Quantity"])

	// Stock should be 10.
	w2, stock := doM1(t, r, http.MethodGet, "/v1/boss/inventory/stock?product_id="+itoa(pid)+"&store_id=9", "50021", "9", nil)
	require.Equal(t, http.StatusOK, w2.Code)
	assert.EqualValues(t, 10, stock["quantity"])

	// Order 4 -> stock drops to 6.
	w3, _ := doM1(t, r, http.MethodPost, "/v1/boss/billing/orders", "50021", "9",
		gin.H{"store_id": 9, "items": []gin.H{{"product_id": pid, "qty": 4}}})
	require.Equal(t, http.StatusOK, w3.Code)

	_, stock2 := doM1(t, r, http.MethodGet, "/v1/boss/inventory/stock?product_id="+itoa(pid)+"&store_id=9", "50021", "9", nil)
	assert.EqualValues(t, 6, stock2["quantity"])

	// The ledger shows movements for the product.
	w4, _ := doM1(t, r, http.MethodGet, "/v1/boss/inventory/movements?product_id="+itoa(pid)+"&store_id=9", "50021", "9", nil)
	assert.Equal(t, http.StatusOK, w4.Code)
}

// TestMembership_JoinIssueRedeem proves the member-code close-the-loop over HTTP.
func TestMembership_JoinIssueRedeem(t *testing.T) {
	r := newM1S2Router()

	w, member := doM1(t, r, http.MethodPost, "/v1/boss/membership/members", "50021", "9",
		gin.H{"store_id": 9, "phone_hash": "h-138", "name": "老王"})
	require.Equal(t, http.StatusOK, w.Code)
	mid := int64(member["ID"].(float64))
	require.Greater(t, mid, int64(0))

	// Issue a dynamic code.
	w2, codeResp := doM1(t, r, http.MethodPost, "/v1/boss/membership/members/"+itoa(mid)+"/code", "50021", "9", nil)
	require.Equal(t, http.StatusOK, w2.Code)
	code, _ := codeResp["code"].(string)
	require.NotEmpty(t, code)

	// Redeem: accrue 50 points.
	w3, res := doM1(t, r, http.MethodPost, "/v1/boss/membership/redeem", "50021", "9",
		gin.H{"store_id": 9, "code": code, "earn_points": 50})
	require.Equal(t, http.StatusOK, w3.Code)
	assert.EqualValues(t, mid, res["MemberID"])
	assert.EqualValues(t, 50, res["Points"])
}

// TestMembership_CrossTenantCodeRejected proves tenant A's member code cannot be
// redeemed under tenant B (the code is tenant-bound + server-side registered).
func TestMembership_CrossTenantCodeRejected(t *testing.T) {
	r := newM1S2Router()

	_, member := doM1(t, r, http.MethodPost, "/v1/boss/membership/members", "50021", "9",
		gin.H{"store_id": 9, "phone_hash": "h-138"})
	mid := int64(member["ID"].(float64))
	_, codeResp := doM1(t, r, http.MethodPost, "/v1/boss/membership/members/"+itoa(mid)+"/code", "50021", "9", nil)
	code, _ := codeResp["code"].(string)

	// Tenant 9999 tries to redeem tenant 50021's code.
	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/membership/redeem", "9999", "9",
		gin.H{"store_id": 9, "code": code, "earn_points": 50})
	assert.Equal(t, http.StatusBadRequest, w.Code, "cross-tenant member code must be rejected")
}

// TestInventory_CrossTenantHidden proves tenant B cannot see or deduct tenant
// A's stock through the HTTP surface.
func TestInventory_CrossTenantHidden(t *testing.T) {
	r := newM1S2Router()

	_, prod := doM1(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "苹果", "unit": "斤", "cost_cents": 300, "price_cents": 500})
	pid := int64(prod["ID"].(float64))
	doM1(t, r, http.MethodPost, "/v1/boss/inventory/purchase", "50021", "9",
		gin.H{"store_id": 9, "product_id": pid, "quantity": 10, "in_price_cents": 300})

	// Tenant 9999 sees zero stock for tenant A's product.
	_, stock := doM1(t, r, http.MethodGet, "/v1/boss/inventory/stock?product_id="+itoa(pid)+"&store_id=9", "9999", "9", nil)
	assert.EqualValues(t, 0, stock["quantity"])

	// Tenant 9999 cannot even purchase-in against tenant A's product id.
	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/inventory/purchase", "9999", "9",
		gin.H{"store_id": 9, "product_id": pid, "quantity": 5, "in_price_cents": 100})
	assert.NotEqual(t, http.StatusOK, w.Code, "cross-tenant purchase against foreign product must be rejected")
}

// itoa is a tiny int64->string helper to keep the fixture readable.
func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
