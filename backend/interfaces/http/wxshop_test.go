package httpserver

// M-C-01 C 端业务接口 HTTP-layer tests (CONTRACT §16): login → 领新人券 → 券包 →
// 动态会员码 → 参团, plus the串户 proof (a tenant-B JWT never sees tenant A's
// data) and the no-token → 401 proof. The router is built over in-memory repos
// with a real (dev-key) JWT stack; the C-side token is minted directly with
// SceneConsumer + an openid (the exact claims the login flow produces).

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/httpx"
	"github.com/zhangguijunshi/backend/pkg/jwt"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// wxMembershipLookuper adapts *membership.Service to the marketing/groupbuy
// MemberLookuper ports (mirrors cmd/api/main.go's membershipLookuper).
type wxMembershipLookuper struct{ svc *membership.Service }

func (l wxMembershipLookuper) Exists(ctx context.Context, tenantID, memberID int64) (bool, error) {
	m, err := l.svc.GetMember(ctx, memberID)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return m.TenantID == tenantID, nil
}

// wxMarginGuard adapts margin.Guard + catalog to marketing.MarginGuard. With no
// products it passes (no negative margin) — enough for the newcomer-coupon flow.
type wxMarginGuard struct {
	guard      margin.Guard
	catalogSvc *catalog.Service
}

func (g wxMarginGuard) ValidateCoupon(ctx context.Context, c *marketing.Coupon) error {
	prods, err := g.catalogSvc.List(ctx, catalog.ProductFilter{Limit: 500})
	if err != nil {
		return nil
	}
	products := make([]margin.ProductView, 0, len(prods))
	for _, p := range prods {
		products = append(products, margin.ProductView{ID: p.ID, CostCents: p.CostCents, PriceCents: p.PriceCents})
	}
	return margin.ValidateCoupon(g.guard, margin.CouponRule{
		Type:             c.Type,
		ThresholdCents:   c.Params.ThresholdCents,
		OffCents:         c.Params.OffCents,
		DiscountPermille: c.Params.DiscountPermille,
	}, products)
}

// wxGroupbuySvc holds the groupbuy service the router was built with so tests
// can seed a squad directly (the router has no boss surface in this fixture).
var wxGroupbuySvc *groupbuy.Service

// gbSvcOf returns the seeded groupbuy service.
func gbSvcOf(_ *gin.Engine) *groupbuy.Service { return wxGroupbuySvc }

// newWxshopRouter builds a router with membership+marketing+groupbuy wired over
// in-memory repos and a real JWT stack, returning the manager so tests can mint
// consumer tokens with a chosen tenant/openid.
func newWxshopRouter(t *testing.T) (*gin.Engine, *jwt.Manager) {
	t.Helper()
	priv, err := jwt.DevKey("wxshop-c-test")
	require.NoError(t, err)
	mgr, err := jwt.New(jwt.Config{PrivateKey: priv, Issuer: "zgjs-test", AccessTTL: time.Minute})
	require.NoError(t, err)
	store := jwt.NewMemorySessionStore()
	t.Cleanup(func() { _ = store.Close() })

	catSvc := catalog.NewService(catalog.NewMemoryProductRepo())
	memSvc := membership.New(membership.NewMemoryMemberRepo(), membership.NewMemoryMemberCoder(nil))
	mktSvc := marketing.New(
		marketing.NewMemoryCouponRepo(),
		marketing.WithMemberLookuper(wxMembershipLookuper{memSvc}),
		marketing.WithMarginGuard(wxMarginGuard{guard: margin.Guard{MarginFloorPct: 10}, catalogSvc: catSvc}),
	)
	gbSvc := groupbuy.New(groupbuy.NewMemoryGroupbuyRepo(), groupbuy.WithMemberLookuper(wxMembershipLookuper{memSvc}))
	wxGroupbuySvc = gbSvc

	return New(Deps{
		Cfg:        &config.Config{App: config.AppConfig{Env: "test"}},
		JWT:        mgr,
		Store:      store,
		Catalog:    catSvc,
		Membership: memSvc,
		Marketing:  mktSvc,
		Groupbuy:   gbSvc,
	}), mgr
}

// mintConsumer signs a SceneConsumer access token for (tenant, openid) — the
// same claims the wx.login flow embeds.
func mintConsumer(t *testing.T, mgr *jwt.Manager, tenantID, storeID int64, openid string) string {
	t.Helper()
	tok, _, err := mgr.Sign(context.Background(), jwt.IssueInput{
		Subject:  "c_1",
		Audience: []string{jwt.SceneConsumer},
		Scene:    jwt.SceneConsumer,
		TenantID: tenantID,
		StoreID:  storeID,
		OpenID:   openid,
	})
	require.NoError(t, err)
	return tok
}

// wxGet issues an authed GET and decodes the envelope.
func wxGet(t *testing.T, r http.Handler, path, bearer string) httpx.Envelope {
	t.Helper()
	return decodeEnvelope(t, doJSON(t, r, http.MethodGet, path, nil, bearer).Body.Bytes())
}

// wxPost issues an authed POST and decodes the envelope.
func wxPost(t *testing.T, r http.Handler, path string, body any, bearer string) httpx.Envelope {
	t.Helper()
	return decodeEnvelope(t, doJSON(t, r, http.MethodPost, path, body, bearer).Body.Bytes())
}

// TestWxshop_FullConsumerFlow proves the happy path: 领新人券 → 券包含该券 →
// 会员档案 → 动态会员码 → 建拼团 → 进行中列表 → 参团.
func TestWxshop_FullConsumerFlow(t *testing.T) {
	r, mgr := newWxshopRouter(t)
	const tenant = 50021
	token := mintConsumer(t, mgr, tenant, 9, "openid-alice")

	// 1. 领新人券 (auto-creates the default 新人券 template + issues it).
	env := wxPost(t, r, "/v1/wxshop/member/claim-newcomer", gin.H{"phone": "13812341234"}, token)
	require.Equal(t, 0, env.Code, "claim-newcomer should succeed: %s", env.Message)
	claim, _ := env.Data.(map[string]any)
	coupon, _ := claim["coupon"].(map[string]any)
	require.NotNil(t, coupon)
	assert.Equal(t, "cash", coupon["type"])
	assert.Equal(t, "¥10", coupon["value_text"])
	assert.Equal(t, "unused", coupon["status"])

	// 2. 领新人券 again → idempotent (already_claimed, no second coupon).
	env = wxPost(t, r, "/v1/wxshop/member/claim-newcomer", gin.H{"phone": "13812341234"}, token)
	require.Equal(t, 0, env.Code)
	claim2, _ := env.Data.(map[string]any)
	assert.Equal(t, true, claim2["already_claimed"], "re-claim must be idempotent")

	// 3. 券包列表 (default unused) contains the newcomer coupon.
	env = wxGet(t, r, "/v1/wxshop/coupons", token)
	require.Equal(t, 0, env.Code)
	list, _ := env.Data.([]any)
	require.Len(t, list, 1, "wallet should hold exactly the one newcomer coupon")
	c0, _ := list[0].(map[string]any)
	assert.Equal(t, "unused", c0["status"])
	assert.Equal(t, "¥10", c0["value_text"])

	// 4. 会员档案.
	env = wxGet(t, r, "/v1/wxshop/member/profile", token)
	require.Equal(t, 0, env.Code)
	prof, _ := env.Data.(map[string]any)
	assert.EqualValues(t, tenant, prof["tenant_id"])
	assert.EqualValues(t, 9, prof["store_id"])
	assert.Equal(t, "普通会员", prof["level_name"])
	memberID := int64(prof["member_id"].(float64))
	require.Positive(t, memberID)

	// 5. 动态会员码.
	env = wxPost(t, r, "/v1/wxshop/member/code", nil, token)
	require.Equal(t, 0, env.Code)
	codeData, _ := env.Data.(map[string]any)
	assert.NotEmpty(t, codeData["code"])
	assert.EqualValues(t, 60, codeData["expire_seconds"])

	// 6. 建拼团 (service-level, tenant ctx) so there is an active squad to join.
	ctx := ctxForTenant(tenant, 9)
	_, err := gbSvcOf(r).Create(ctx, groupbuyCreateInput())
	require.NoError(t, err)

	// 7. 进行中拼团列表 (store-scoped).
	env = wxGet(t, r, "/v1/wxshop/groupbuys/active", token)
	require.Equal(t, 0, env.Code)
	gbs, _ := env.Data.([]any)
	require.Len(t, gbs, 1)
	g0, _ := gbs[0].(map[string]any)
	gbID := int64(g0["id"].(float64))
	assert.EqualValues(t, 3, g0["required"])
	assert.EqualValues(t, 1990, g0["price_cents"])

	// 8. 参团.
	env = wxPost(t, r, "/v1/wxshop/groupbuys/"+itoa(gbID)+"/join", nil, token)
	require.Equal(t, 0, env.Code, "groupbuy join should succeed: %s", env.Message)
	joinData, _ := env.Data.(map[string]any)
	require.NotNil(t, joinData["participant"])

	// 9. 重复参团 → 60015.
	env = wxPost(t, r, "/v1/wxshop/groupbuys/"+itoa(gbID)+"/join", nil, token)
	assert.Equal(t, 60015, env.Code, "double join should 60015")

	// 10. 订单 (degraded empty list, CONTRACT §16.8).
	env = wxGet(t, r, "/v1/wxshop/orders", token)
	require.Equal(t, 0, env.Code)
	orders, _ := env.Data.([]any)
	assert.Empty(t, orders)
}

// TestWxshop_CrossTenantIsolation proves tenant A's member/coupon data is never
// visible to a tenant-B JWT: B gets its own empty wallet/profile, not A's.
func TestWxshop_CrossTenantIsolation(t *testing.T) {
	r, mgr := newWxshopRouter(t)
	const tenantA, tenantB = 50021, 50022
	// Same openid on both tenants (a WeChat user can be a member of many shops).
	tokenA := mintConsumer(t, mgr, tenantA, 9, "openid-shared")
	tokenB := mintConsumer(t, mgr, tenantB, 9, "openid-shared")

	// Tenant A claims a newcomer coupon.
	env := wxPost(t, r, "/v1/wxshop/member/claim-newcomer", gin.H{"phone": "13800000000"}, tokenA)
	require.Equal(t, 0, env.Code)
	profA, _ := wxGet(t, r, "/v1/wxshop/member/profile", tokenA).Data.(map[string]any)
	memberA := int64(profA["member_id"].(float64))

	// Tenant A's wallet has 1 coupon.
	listA, _ := wxGet(t, r, "/v1/wxshop/coupons", tokenA).Data.([]any)
	require.Len(t, listA, 1)

	// Tenant B's wallet is EMPTY (does NOT see A's coupon). B has not joined yet,
	// so B's first access enrols a distinct member under tenant B.
	envB := wxGet(t, r, "/v1/wxshop/coupons", tokenB)
	require.Equal(t, 0, envB.Code)
	listB, _ := envB.Data.([]any)
	assert.Empty(t, listB, "tenant B must not see tenant A's coupons")

	// Tenant B's profile is a DIFFERENT member id (tenant-scoped row), proving B
	// did not ride on A's member row even with the same openid.
	profB, _ := wxGet(t, r, "/v1/wxshop/member/profile", tokenB).Data.(map[string]any)
	memberB := int64(profB["member_id"].(float64))
	assert.EqualValues(t, tenantB, profB["tenant_id"])
	assert.NotEqual(t, memberA, memberB, "same openid must map to per-tenant member rows")

	// Tenant B cannot join a squad that lives only in tenant A (40404).
	ctxA := ctxForTenant(tenantA, 9)
	g, err := gbSvcOf(r).Create(ctxA, groupbuyCreateInput())
	require.NoError(t, err)
	envJoin := wxPost(t, r, "/v1/wxshop/groupbuys/"+itoa(g.ID)+"/join", nil, tokenB)
	assert.Equal(t, 40404, envJoin.Code, "tenant B joining tenant A's squad must 40404")
}

// TestWxshop_NoToken proves every C-side endpoint rejects an unauthenticated
// caller with 401 (errcode 40101).
func TestWxshop_NoToken(t *testing.T) {
	r, _ := newWxshopRouter(t)
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, "/v1/wxshop/coupons"},
		{http.MethodGet, "/v1/wxshop/member/profile"},
		{http.MethodPost, "/v1/wxshop/member/code"},
		{http.MethodPost, "/v1/wxshop/member/claim-newcomer"},
		{http.MethodGet, "/v1/wxshop/groupbuys/active"},
		{http.MethodPost, "/v1/wxshop/groupbuys/1/join"},
		{http.MethodGet, "/v1/wxshop/orders"},
	} {
		w := doJSON(t, r, p.method, p.path, nil, "")
		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s %s should 401", p.method, p.path)
		env := decodeEnvelope(t, w.Body.Bytes())
		assert.Equal(t, 40101, env.Code, "%s %s should 40101", p.method, p.path)
	}
}

// --- test fixtures -----------------------------------------------------------

// ctxForTenant builds a system context for direct service calls (seeding a squad).
func ctxForTenant(tenantID, storeID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		Source:   multitenancy.SourceSystem,
	})
}

func groupbuyCreateInput() groupbuy.CreateInput {
	return groupbuy.CreateInput{
		StoreID:       9,
		ProductID:     1,
		Title:         "3人团 新鲜草莓 2斤装",
		Params:        groupbuy.GroupbuyParams{PriceCents: 1990},
		RequiredCount: 3,
		ValidHours:    24,
	}
}
