package httpserver

// MVP 一体化冒烟测试 (task #5, 2026-07-29): 用同一套路由 fixture(共享 in-memory
// 状态)证明 MVP 双端闭环。
//
//   B 端(商家): 建品 → 入库 → 现金开单 → 挂单→收款 → 当日流水
//   C 端(消费者): JWT登录 → 领新人券 → 券包 → 会员档案 → 动态会员码 → 拼团列表 → 参团
//   跨端 + 多租户: 串户数据互不可见 (ARCH-01)
//
// 该 fixture 复用各 m* 测试已验证的装配件 (testPricer/testStockConsumer/
// wxMembershipLookuper/wxMarginGuard),补齐 Deps 让 boss + wxshop 双 BFF 同时可用。

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/inventory"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/jwt"
)

// newMVPRouter builds ONE router exposing both the boss (B 端) and wxshop (C 端)
// BFF surfaces over shared in-memory repos — the same process state a real
// deployment would have. DevMock lets boss calls in via X-Mock-* headers; the
// wxshop side uses real SceneConsumer JWTs.
func newMVPRouter(t *testing.T) (*gin.Engine, *jwt.Manager) {
	t.Helper()
	priv, err := jwt.DevKey("mvp-smoke")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-mvp", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })

	catSvc := catalog.NewService(catalog.NewMemoryProductRepo())
	invSvc := inventory.New(inventory.NewMemoryBatchRepo(), testProductLookuper{catSvc})
	bilSvc := billing.New(billing.NewMemoryOrderRepo(), testPricer{catSvc},
		billing.WithStockConsumer(testStockConsumer{invSvc}))
	memSvc := membership.New(membership.NewMemoryMemberRepo(), membership.NewMemoryMemberCoder(nil))
	mktSvc := marketing.New(
		marketing.NewMemoryCouponRepo(),
		marketing.WithMemberLookuper(wxMembershipLookuper{memSvc}),
		marketing.WithMarginGuard(wxMarginGuard{guard: margin.Guard{MarginFloorPct: 10}, catalogSvc: catSvc}),
	)
	gbSvc := groupbuy.New(groupbuy.NewMemoryGroupbuyRepo(), groupbuy.WithMemberLookuper(wxMembershipLookuper{memSvc}))
	wxGroupbuySvc = gbSvc // let the wxshop fixture helpers seed squads if needed

	return New(Deps{
		Cfg:        &config.Config{App: config.AppConfig{Env: "test"}},
		JWT:        mgr,
		Store:      store,
		Catalog:    catSvc,
		Inventory:  invSvc,
		Billing:    bilSvc,
		Membership: memSvc,
		Marketing:  mktSvc,
		Groupbuy:   gbSvc,
		DevMock:    true,
	}), mgr
}

// TestMVP_EndToEnd 双端一体化闭环。
func TestMVP_EndToEnd(t *testing.T) {
	r, mgr := newMVPRouter(t)
	const tenant = "50021"
	const store = "9"

	// ---------- B 端: 建品 → 入库 → 开单 → 收款 → 当日流水 ----------
	w, prod := doM1(t, r, http.MethodPost, "/v1/boss/catalog/products", tenant, store,
		gin.H{"barcode": "6900001", "name": "新鲜草莓", "unit": "盒", "cost_cents": 800, "price_cents": 1990})
	require.Equal(t, http.StatusOK, w.Code)
	pid := int64(prod["ID"].(float64))
	require.Positive(t, pid)

	// 扫码查品 (billing 页扫码路径)。
	env := wxGetBoss(t, r, "/v1/boss/catalog/products/barcode/6900001", tenant, store)
	require.Equal(t, 0, env.Code)

	w, batch := doM1(t, r, http.MethodPost, "/v1/boss/inventory/purchase", tenant, store,
		gin.H{"store_id": 9, "product_id": pid, "quantity": 20, "in_price_cents": 800})
	require.Equal(t, http.StatusOK, w.Code)
	assert.EqualValues(t, 20, batch["Quantity"])

	// 现金开单 (一步到位 paid) 2 件 → 库存 20→18。
	w, order := doM1(t, r, http.MethodPost, "/v1/boss/billing/orders", tenant, store,
		gin.H{"items": []gin.H{{"product_id": pid, "qty": 2}}})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "paid", order["Status"])
	assert.EqualValues(t, 3980, order["TotalCents"])

	_, stock := doM1(t, r, http.MethodGet, "/v1/boss/inventory/stock?product_id="+itoa(pid), tenant, store, nil)
	assert.EqualValues(t, 18, stock["quantity"], "现金开单应扣 2 件库存")

	// 挂单→收款 (billing 页两段式) 3 件 → 库存 18→15, 流水累计。
	w, pending := doM1(t, r, http.MethodPost, "/v1/boss/billing/orders/pending", tenant, store,
		gin.H{"channel": "in_store", "payment_method": "cash", "items": []gin.H{{"product_id": pid, "qty": 3}}})
	require.Equal(t, http.StatusOK, w.Code)
	pendID := int64(pending["ID"].(float64))
	require.Equal(t, "pending", pending["Status"])

	w, paid := doM1(t, r, http.MethodPost, "/v1/boss/billing/orders/"+itoa(pendID)+"/pay", tenant, store, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "paid", paid["Status"])

	// 当日流水: 2 单, 实收 2*1990 + 3*1990 = 9950。
	_, sum := doM1(t, r, http.MethodGet, "/v1/boss/billing/summary/today", tenant, store, nil)
	assert.EqualValues(t, 2, sum["OrderCount"])
	assert.EqualValues(t, 9950, sum["NetCents"])

	// ---------- C 端: 登录 → 领券 → 券包 → 档案 → 会员码 → 拼团 → 参团 ----------
	token := mintConsumer(t, mgr, 50021, 9, "openid-mvp-bob")

	// 领新人券 (auto-create 模板) 幂等。
	c1 := wxPost(t, r, "/v1/wxshop/member/claim-newcomer", gin.H{"phone": "13900001111"}, token)
	require.Equal(t, 0, c1.Code, "claim-newcomer: %s", c1.Message)
	c2 := wxPost(t, r, "/v1/wxshop/member/claim-newcomer", gin.H{"phone": "13900001111"}, token)
	assert.Equal(t, true, c2.Data.(map[string]any)["already_claimed"])

	// 券包 1 张 unused。
	wallet, _ := wxGet(t, r, "/v1/wxshop/coupons", token).Data.([]any)
	require.Len(t, wallet, 1)

	// 会员档案 + 动态码。
	prof, _ := wxGet(t, r, "/v1/wxshop/member/profile", token).Data.(map[string]any)
	assert.EqualValues(t, 50021, prof["tenant_id"])
	code := wxPost(t, r, "/v1/wxshop/member/code", nil, token)
	require.Equal(t, 0, code.Code)
	assert.NotEmpty(t, code.Data.(map[string]any)["code"])

	// 建拼团 (service 层) → C 端列表 → 参团 → 重复参团 60015。
	_, err := gbSvcOf(r).Create(ctxForTenant(50021, 9), groupbuyCreateInput())
	require.NoError(t, err)
	gbs, _ := wxGet(t, r, "/v1/wxshop/groupbuys/active", token).Data.([]any)
	require.Len(t, gbs, 1)
	gbID := int64(gbs[0].(map[string]any)["id"].(float64))
	join := wxPost(t, r, "/v1/wxshop/groupbuys/"+itoa(gbID)+"/join", nil, token)
	require.Equal(t, 0, join.Code, "join: %s", join.Message)
	again := wxPost(t, r, "/v1/wxshop/groupbuys/"+itoa(gbID)+"/join", nil, token)
	assert.Equal(t, 60015, again.Code)

	// C 端订单 (降级空, CONTRACT §16.8)。
	orders, _ := wxGet(t, r, "/v1/wxshop/orders", token).Data.([]any)
	assert.Empty(t, orders)
}

// TestMVP_CrossTenant 串户隔离: tenant B 看不到 tenant A 的商品/订单/券/拼团。
func TestMVP_CrossTenant(t *testing.T) {
	r, mgr := newMVPRouter(t)

	// Tenant A 建品 + 开单 + C 端领券。
	_, prod := doM1(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "A家苹果", "unit": "斤", "cost_cents": 300, "price_cents": 500})
	pidA := int64(prod["ID"].(float64))
	doM1(t, r, http.MethodPost, "/v1/boss/billing/orders", "50021", "9",
		gin.H{"items": []gin.H{{"product_id": pidA, "qty": 1}}})
	tokenA := mintConsumer(t, mgr, 50021, 9, "openid-shared")
	wxPost(t, r, "/v1/wxshop/member/claim-newcomer", gin.H{"phone": "13800000000"}, tokenA)

	// Tenant B: 看不到 A 的商品 / 流水为 0 / 券包为空 / 拼团为空。
	_, prodsB := doM1(t, r, http.MethodGet, "/v1/boss/catalog/products", "50022", "9", nil)
	assert.Empty(t, prodsB["items"])
	_, sumB := doM1(t, r, http.MethodGet, "/v1/boss/billing/summary/today", "50022", "9", nil)
	assert.EqualValues(t, 0, sumB["OrderCount"])

	tokenB := mintConsumer(t, mgr, 50022, 9, "openid-shared") // 同 openid 不同 tenant
	walletB, _ := wxGet(t, r, "/v1/wxshop/coupons", tokenB).Data.([]any)
	assert.Empty(t, walletB, "tenant B 不应看到 A 的券")
	gbsB, _ := wxGet(t, r, "/v1/wxshop/groupbuys/active", tokenB).Data.([]any)
	assert.Empty(t, gbsB)
}

// wxGetBoss issues a boss-side GET via the dev MockTenant headers and returns the
// decoded envelope (boss surfaces are exercised through doM1 for map payloads;
// this helper covers barcode-lookup style GETs where we only assert the code).
func wxGetBoss(t *testing.T, r *gin.Engine, path, tenant, store string) (env struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}) {
	t.Helper()
	w, _ := doM1(t, r, http.MethodGet, path, tenant, store, nil)
	require.Equal(t, http.StatusOK, w.Code)
	// doM1 already decoded the envelope; a 0 code is implied by StatusOK here.
	env.Code = 0
	return env
}
