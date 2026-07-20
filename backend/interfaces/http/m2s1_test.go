package httpserver

// M2-S1 HTTP-layer tests (CONTRACT §10.6): the marketing boss loop
//   建品 → 建会员 → 建券 → 上线 → 发券 → 核销
// plus the cross-tenant proof (tenant B cannot read/issue/redeem tenant A's
// coupons) and the margin-guard demonstration (a below-floor coupon is rejected
// with 60001).

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/catalog"
	"github.com/zhangguijunshi/backend/internal/margin"
	"github.com/zhangguijunshi/backend/internal/marketing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/pkg/config"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// m2s1MembershipLookuper adapts *membership.Service to marketing.MemberLookuper
// for the test fixture (mirrors cmd/api/main.go's membershipLookuper).
type m2s1MembershipLookuper struct{ svc *membership.Service }

func (l m2s1MembershipLookuper) Exists(ctx context.Context, tenantID, memberID int64) (bool, error) {
	m, err := l.svc.GetMember(ctx, memberID)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return m.TenantID == tenantID, nil
}

// m2s1MarginGuard adapts margin.Guard + *catalog.Service to marketing.MarginGuard
// for the test fixture (mirrors cmd/api/main.go's catalogMarginGuard).
type m2s1MarginGuard struct {
	guard      margin.Guard
	catalogSvc *catalog.Service
}

func (g m2s1MarginGuard) ValidateCoupon(ctx context.Context, c *marketing.Coupon) error {
	prods, err := g.catalogSvc.List(ctx, catalog.ProductFilter{Limit: 500})
	if err != nil {
		return nil
	}
	products := make([]margin.ProductView, 0, len(prods))
	for _, p := range prods {
		products = append(products, margin.ProductView{ID: p.ID, CostCents: p.CostCents, PriceCents: p.PriceCents})
	}
	rule := margin.CouponRule{
		Type:             c.Type,
		ThresholdCents:   c.Params.ThresholdCents,
		OffCents:         c.Params.OffCents,
		DiscountPermille: c.Params.DiscountPermille,
	}
	return margin.ValidateCoupon(g.guard, rule, products)
}

// newM2S1Router builds a router with catalog+membership+marketing wired over
// in-memory repos and the dev MockTenant header enabled. The marketing service
// carries the margin guard (floor 10%) + member lookuper.
func newM2S1Router() *gin.Engine {
	catSvc := catalog.NewService(catalog.NewMemoryProductRepo())
	memSvc := membership.New(membership.NewMemoryMemberRepo(), membership.NewMemoryMemberCoder(nil))
	mktSvc := marketing.New(
		marketing.NewMemoryCouponRepo(),
		marketing.WithMemberLookuper(m2s1MembershipLookuper{memSvc}),
		marketing.WithMarginGuard(m2s1MarginGuard{
			guard:      margin.Guard{MarginFloorPct: 10},
			catalogSvc: catSvc,
		}),
	)
	return New(Deps{
		Cfg:        &config.Config{App: config.AppConfig{Env: "test"}},
		Catalog:    catSvc,
		Membership: memSvc,
		Marketing:  mktSvc,
		DevMock:    true,
	})
}

// doM2 issues a request with the mock tenant/store headers and returns the
// envelope + the data field decoded as a map (single-object responses).
func doM2(t *testing.T, r *gin.Engine, method, path, tenant, store string, body any) (env httpx.Envelope, data map[string]any) {
	t.Helper()
	w, _ := doM1(t, r, method, path, tenant, store, body)
	env = decodeEnvelope(t, w.Body.Bytes())
	data, _ = env.Data.(map[string]any)
	return env, data
}

// TestMarketing_CreateIssueRedeem proves the full boss loop: create a product,
// enrol a member, create a discount coupon, activate it, issue it to the
// member (margin guard passes), and redeem it (unused→used).
func TestMarketing_CreateIssueRedeem(t *testing.T) {
	r := newM2S1Router()

	// 1. Create a product with a healthy margin (cost 300, price 500 → 40%).
	_, prod := doM2(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "苹果", "unit": "斤", "cost_cents": 300, "price_cents": 500})
	pid := int64(prod["ID"].(float64))

	// 2. Enrol a member.
	_, mem := doM2(t, r, http.MethodPost, "/v1/boss/membership/members", "50021", "9",
		gin.H{"phone_hash": "hash-memb-1", "name": "老王"})
	memberID := int64(mem["ID"].(float64))

	// 3. Create an 8.5折 discount coupon (permille 850). On price 500: pay=425,
	//    margin=(425-300)/425=29% > floor 10% → passes the guard.
	env, coupon := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons", "50021", "9",
		gin.H{
			"name": "苹果8.5折", "type": "discount",
			"params":   gin.H{"discount_permille": 850},
			"valid_to": "2099-12-31T23:59:59Z",
		})
	require.Equal(t, 0, env.Code, "create coupon should succeed: %s", env.Message)
	assert.Equal(t, "draft", coupon["Status"])
	couponID := int64(coupon["ID"].(float64))

	// 4. Activate the coupon.
	env, _ = doM2(t, r, http.MethodPatch, "/v1/boss/marketing/coupons/"+itoa(couponID)+"/status", "50021", "9",
		gin.H{"status": "active"})
	require.Equal(t, 0, env.Code, "activate should succeed: %s", env.Message)

	// 5. Issue to the member.
	env, mc := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons/issue", "50021", "9",
		gin.H{"member_id": memberID, "coupon_id": couponID, "source": "manual"})
	require.Equal(t, 0, env.Code, "issue should succeed: %s", env.Message)
	assert.Equal(t, "unused", mc["Status"])
	mcID := int64(mc["ID"].(float64))

	// 6. Redeem → used.
	env, mc2 := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons/redeem", "50021", "9",
		gin.H{"member_coupon_id": mcID, "ref_order_id": 9001})
	require.Equal(t, 0, env.Code, "redeem should succeed: %s", env.Message)
	assert.Equal(t, "used", mc2["Status"])

	// 7. List member's coupons (unused should be empty now, used has 1).
	w, _ := doM1(t, r, http.MethodGet, "/v1/boss/marketing/members/"+itoa(memberID)+"/coupons?status=unused", "50021", "9", nil)
	envList := decodeEnvelope(t, w.Body.Bytes())
	list, _ := envList.Data.([]any)
	assert.Equal(t, 0, envList.Code)
	assert.Empty(t, list, "no unused coupons after redeem")

	// product is referenced (suppress unused-var); pid confirmed by the issue flow
	_ = pid
}

// TestMarketing_CrossTenantRejected proves tenant B cannot read, issue against,
// or redeem tenant A's coupons.
func TestMarketing_CrossTenantRejected(t *testing.T) {
	r := newM2S1Router()

	// Tenant A: product + member + active coupon + issued member-coupon.
	doM2(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "梨", "unit": "斤", "cost_cents": 200, "price_cents": 400})
	_, mem := doM2(t, r, http.MethodPost, "/v1/boss/membership/members", "50021", "9",
		gin.H{"phone_hash": "hash-x", "name": "老李"})
	memberID := int64(mem["ID"].(float64))
	_, coupon := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons", "50021", "9",
		gin.H{"name": "梨券", "type": "cash", "params": gin.H{"off_cents": 50}, "valid_to": "2099-12-31T23:59:59Z"})
	couponID := int64(coupon["ID"].(float64))
	doM2(t, r, http.MethodPatch, "/v1/boss/marketing/coupons/"+itoa(couponID)+"/status", "50021", "9",
		gin.H{"status": "active"})
	_, mc := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons/issue", "50021", "9",
		gin.H{"member_id": memberID, "coupon_id": couponID})
	mcID := int64(mc["ID"].(float64))

	// Tenant B (50022) cannot read tenant A's coupon.
	w, _ := doM1(t, r, http.MethodGet, "/v1/boss/marketing/coupons/"+itoa(couponID), "50022", "9", nil)
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 40403, env.Code, "cross-tenant get coupon should 40403")

	// Tenant B enrolls its OWN member, then tries to issue tenant A's coupon.
	// The member check passes (member exists in tenant B); the coupon lookup
	// fails (coupon belongs to tenant A) → 40403.
	_, memB := doM2(t, r, http.MethodPost, "/v1/boss/membership/members", "50022", "9",
		gin.H{"phone_hash": "hash-b-tenant", "name": "租户B会员"})
	memberBID := int64(memB["ID"].(float64))
	w, _ = doM1(t, r, http.MethodPost, "/v1/boss/marketing/coupons/issue", "50022", "9",
		gin.H{"member_id": memberBID, "coupon_id": couponID})
	env = decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 40403, env.Code, "cross-tenant issue should 40403")

	// Tenant B cannot redeem tenant A's member-coupon.
	w, _ = doM1(t, r, http.MethodPost, "/v1/boss/marketing/coupons/redeem", "50022", "9",
		gin.H{"member_coupon_id": mcID})
	env = decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 40403, env.Code, "cross-tenant redeem should 40403")
}

// TestMarketing_MarginGuardRejects proves the gross-margin guardrail rejects a
// below-floor coupon issuance. A 5折 discount on price 500 / cost 300 yields
// pay=250, margin=(250-300)/250=-20% < 10% floor → 60001.
func TestMarketing_MarginGuardRejects(t *testing.T) {
	r := newM2S1Router()

	// Product with 40% margin.
	doM2(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "芒果", "unit": "斤", "cost_cents": 300, "price_cents": 500})
	_, mem := doM2(t, r, http.MethodPost, "/v1/boss/membership/members", "50021", "9",
		gin.H{"phone_hash": "hash-mg", "name": "老陈"})
	memberID := int64(mem["ID"].(float64))

	// 5折 coupon (permille 500): pay=250, margin=-20% → below floor.
	_, coupon := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons", "50021", "9",
		gin.H{"name": "芒果5折", "type": "discount", "params": gin.H{"discount_permille": 500}, "valid_to": "2099-12-31T23:59:59Z"})
	couponID := int64(coupon["ID"].(float64))
	doM2(t, r, http.MethodPatch, "/v1/boss/marketing/coupons/"+itoa(couponID)+"/status", "50021", "9",
		gin.H{"status": "active"})

	// Issue → margin guard rejects with 60001.
	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/marketing/coupons/issue", "50021", "9",
		gin.H{"member_id": memberID, "coupon_id": couponID})
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 60001, env.Code, "below-floor coupon should be rejected by margin guard: %s", env.Message)
}

// TestMarketing_QuotaExhausted proves the quota guard rejects over-issuance.
func TestMarketing_QuotaExhausted(t *testing.T) {
	r := newM2S1Router()
	doM2(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "葡萄", "unit": "斤", "cost_cents": 200, "price_cents": 400})
	_, m1 := doM2(t, r, http.MethodPost, "/v1/boss/membership/members", "50021", "9",
		gin.H{"phone_hash": "h1", "name": "甲"})
	_, m2 := doM2(t, r, http.MethodPost, "/v1/boss/membership/members", "50021", "9",
		gin.H{"phone_hash": "h2", "name": "乙"})
	id1 := int64(m1["ID"].(float64))
	id2 := int64(m2["ID"].(float64))

	_, coupon := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons", "50021", "9",
		gin.H{"name": "葡萄券", "type": "cash", "params": gin.H{"off_cents": 10}, "valid_to": "2099-12-31T23:59:59Z", "total_quota": 1})
	couponID := int64(coupon["ID"].(float64))
	doM2(t, r, http.MethodPatch, "/v1/boss/marketing/coupons/"+itoa(couponID)+"/status", "50021", "9",
		gin.H{"status": "active"})

	// 1st issue OK.
	env, _ := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons/issue", "50021", "9",
		gin.H{"member_id": id1, "coupon_id": couponID})
	assert.Equal(t, 0, env.Code)

	// 2nd issue → quota exhausted (60005).
	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/marketing/coupons/issue", "50021", "9",
		gin.H{"member_id": id2, "coupon_id": couponID})
	env = decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 60005, env.Code, "over-quota issue should 60005: %s", env.Message)
}

// TestMarketing_RedeemTwiceRejected proves a used coupon cannot be redeemed again.
func TestMarketing_RedeemTwiceRejected(t *testing.T) {
	r := newM2S1Router()
	doM2(t, r, http.MethodPost, "/v1/boss/catalog/products", "50021", "9",
		gin.H{"name": "桃", "unit": "斤", "cost_cents": 200, "price_cents": 400})
	_, mem := doM2(t, r, http.MethodPost, "/v1/boss/membership/members", "50021", "9",
		gin.H{"phone_hash": "h-twice", "name": "老赵"})
	memberID := int64(mem["ID"].(float64))
	_, coupon := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons", "50021", "9",
		gin.H{"name": "桃券", "type": "cash", "params": gin.H{"off_cents": 10}, "valid_to": "2099-12-31T23:59:59Z"})
	couponID := int64(coupon["ID"].(float64))
	doM2(t, r, http.MethodPatch, "/v1/boss/marketing/coupons/"+itoa(couponID)+"/status", "50021", "9",
		gin.H{"status": "active"})
	_, mc := doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons/issue", "50021", "9",
		gin.H{"member_id": memberID, "coupon_id": couponID})
	mcID := int64(mc["ID"].(float64))

	// 1st redeem OK.
	doM2(t, r, http.MethodPost, "/v1/boss/marketing/coupons/redeem", "50021", "9",
		gin.H{"member_coupon_id": mcID})

	// 2nd redeem → already used (60008).
	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/marketing/coupons/redeem", "50021", "9",
		gin.H{"member_coupon_id": mcID})
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 60008, env.Code, "double redeem should 60008: %s", env.Message)
}
