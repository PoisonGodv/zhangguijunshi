package httpserver

// M4-S1 HTTP-layer smoke tests (CONTRACT §15.7):
//   ① Sub-merchant onboarding → query → active (FakeWxPay 即时 transition).
//   ② Order pending → prepay → WeChat notify → order paid + payment_ref linked.
//   ③ Notify idempotency — a repeat callback keeps the ledger at one row and
//      does not re-fulfil (changed flag gates Fulfiller).
//   ④ Recharge prepay → notify → membership.Recharge credits balance + gift
//      (反二清 fulfilment chain: fund cleared to sub-merchant, book balance added).
//   ⑤ Regional cap rejects an over-cap recharge (errcode 60003), balance unchanged.
//   ⑥ Subscription paywall — free tenant denied ai_advice; after Subscribe pro
//      the feature is allowed.
//   ⑦ Cross-tenant GET payment → 40403 (absence == foreign ownership, ARCH-01).
//
// Response structs carry no JSON tags (matching billing/membership's existing
// style), so their fields surface as PascalCase in the decoded envelope; the
// gin.H literals the handlers build (feature/allowed/payment/prepay) stay the
// snake_case keys written in code.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/internal/payment"
	"github.com/zhangguijunshi/backend/internal/subscription"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// newM4S1Router wires the M4-S1 services over in-memory repos with DevMock on.
// It returns the memberRepo so a recharge test can assert the stored balance
// directly (no HTTP account-read endpoint exists yet).
func newM4S1Router() (*gin.Engine, *billing.Service, *membership.Service, *payment.Service, *subscription.Service, *membership.MemoryMemberRepo) {
	billSvc := billing.New(billing.NewMemoryOrderRepo(), m3s3Pricer{priceCents: 500})
	memberRepo := membership.NewMemoryMemberRepo()
	mbrSvc := membership.New(memberRepo, membership.NewMemoryMemberCoder(nil))
	subSvc := subscription.NewService(subscription.NewMemoryStore())
	fakeWx := payment.NewFakeWxPay()
	paySvc := payment.NewService(
		payment.NewMemorySubMerchantRepo(), payment.NewMemoryPaymentRepo(), payment.NewMemoryLedgerRepo(),
		payment.WithApplier(fakeWx), payment.WithPreparer(fakeWx), payment.WithNotifyParser(fakeWx),
		payment.WithFulfiller(m4s1Fulfiller{bill: billSvc, mbr: mbrSvc, sub: subSvc}),
	)
	r := New(Deps{
		Cfg:          &config.Config{App: config.AppConfig{Env: "test"}},
		Billing:      billSvc,
		Membership:   mbrSvc,
		Payment:      paySvc,
		Subscription: subSvc,
		DevMock:      true,
	})
	return r, billSvc, mbrSvc, paySvc, subSvc, memberRepo
}

// m4s1Fulfiller mirrors cmd/api.paymentFulfiller (package main, cannot be
// imported here): it dispatches a successful payment to the fulfilment domain
// named by Subject.
type m4s1Fulfiller struct {
	bill *billing.Service
	mbr  *membership.Service
	sub  *subscription.Service
}

func (f m4s1Fulfiller) Fulfill(ctx context.Context, p *payment.Payment) error {
	switch p.Subject {
	case payment.SubjectOrder:
		if f.bill == nil || p.PaidAt == nil {
			return nil
		}
		_, err := f.bill.MarkOrderPaid(ctx, p.RefID, p.OutTradeNo, *p.PaidAt)
		return err
	case payment.SubjectRecharge:
		if f.mbr == nil {
			return nil
		}
		_, err := f.mbr.Recharge(ctx, membership.RechargeInput{
			MemberID:     p.RefID,
			AmountCents:  p.AmountCents,
			RefPaymentID: p.ID,
		})
		return err
	case payment.SubjectSubscription:
		if f.sub == nil {
			return nil
		}
		_, err := f.sub.ConfirmPayment(ctx, p.TenantID, p.AmountCents, p.Provider, p.OutTradeNo)
		return err
	}
	return nil
}

// postNotify POSTs a FakeWxPay-shaped notify body to the unauthenticated notify
// endpoint and returns the WeChat {code} field (SUCCESS / FAIL).
func postNotify(t *testing.T, r *gin.Engine, body any) string {
	t.Helper()
	buf, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/v1/wxshop/payment/notify", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if c, ok := resp["code"].(string); ok {
		return c
	}
	return ""
}

// idOf reads the PascalCase "ID" field a response struct serialises to.
func idOf(t *testing.T, data map[string]any) int64 {
	require.NotNil(t, data, "empty response data")
	id, ok := data["ID"].(float64)
	require.True(t, ok, "no ID field: %v", data)
	return int64(id)
}

// payOf reads the "payment" object the prepay handler returns.
func payOf(t *testing.T, data map[string]any) map[string]any {
	p, ok := data["payment"].(map[string]any)
	require.True(t, ok, "no payment in response: %v", data)
	return p
}

// onboardActiveSM creates a sub-merchant for tenant/store and polls it to
// active (FakeWxPay activates on first query). Returns its id.
func onboardActiveSM(t *testing.T, r *gin.Engine, tenant, store string) int64 {
	_, data := doM1(t, r, "POST", "/v1/boss/payment/sub-merchants", tenant, store, gin.H{
		"business_name": "测试店",
		"license_no":    "LIC-X",
		"provider":      "wxpay",
	})
	id := idOf(t, data)
	_, q := doM1(t, r, "POST", fmt.Sprintf("/v1/boss/payment/sub-merchants/%d/query", id), tenant, store, nil)
	require.Equal(t, "active", q["Status"])
	return id
}

func TestM4S1_SubMerchantOnboarding(t *testing.T) {
	r, _, _, _, _, _ := newM4S1Router()
	id := onboardActiveSM(t, r, "1", "1")
	require.Greater(t, id, int64(0))
}

func TestM4S1_OrderPayNotifyAndFulfill(t *testing.T) {
	r, _, _, paySvc, _, _ := newM4S1Router()
	onboardActiveSM(t, r, "1", "1")

	// pending order (online-pay flow).
	_, od := doM1(t, r, "POST", "/v1/boss/billing/orders/pending", "1", "1", gin.H{
		"items": []gin.H{{"product_id": 1, "qty": 2}},
	})
	require.Equal(t, "pending", od["Status"])
	orderID := idOf(t, od)

	// prepay (subject=order).
	_, pp := doM1(t, r, "POST", "/v1/boss/payment/prepay", "1", "1", gin.H{
		"subject": "order", "ref_id": orderID, "amount_cents": 1000,
	})
	outTradeNo, ok := payOf(t, pp)["OutTradeNo"].(string)
	require.True(t, ok)

	// notify → order paid + fulfilment.
	notifyBody := gin.H{
		"out_trade_no": outTradeNo, "transaction_id": "wx-tx-ord-1",
		"amount_total": 1000, "trade_state": "SUCCESS", "success_time": "2026-07-28T12:00:00Z",
	}
	require.Equal(t, "SUCCESS", postNotify(t, r, notifyBody))
	_, got := doM1(t, r, "GET", fmt.Sprintf("/v1/boss/billing/orders/%d", orderID), "1", "1", nil)
	require.Equal(t, "paid", got["Status"])
	require.Equal(t, outTradeNo, got["payment_ref"]) // Order.PaymentRef carries a json tag

	// ledger exactly one row.
	led, err := paySvc.ListLedger(m3s3TenantCtx(context.Background(), 1), 10)
	require.NoError(t, err)
	require.Len(t, led, 1)

	// idempotent: repeat callback → ledger still one, no re-fulfil.
	require.Equal(t, "SUCCESS", postNotify(t, r, notifyBody))
	led2, _ := paySvc.ListLedger(m3s3TenantCtx(context.Background(), 1), 10)
	require.Len(t, led2, 1)
}

func TestM4S1_RechargeNotifyAndFulfill(t *testing.T) {
	r, _, _, _, _, memberRepo := newM4S1Router()
	onboardActiveSM(t, r, "1", "1")

	_, md := doM1(t, r, "POST", "/v1/boss/membership/members", "1", "1", gin.H{"phone_hash": "ph-recharge"})
	memberID := idOf(t, md)

	// prepay recharge 20000 (充200送20 → gift 2000).
	_, pp := doM1(t, r, "POST", "/v1/boss/payment/prepay", "1", "1", gin.H{
		"subject": "recharge", "ref_id": memberID, "amount_cents": 20000,
	})
	outTradeNo, ok := payOf(t, pp)["OutTradeNo"].(string)
	require.True(t, ok)

	// notify → membership.Recharge credits balance + gift.
	require.Equal(t, "SUCCESS", postNotify(t, r, gin.H{
		"out_trade_no": outTradeNo, "transaction_id": "wx-tx-rch-1",
		"amount_total": 20000, "trade_state": "SUCCESS", "success_time": "2026-07-28T12:00:00Z",
	}))
	acc, err := memberRepo.GetAccount(m3s3TenantCtx(context.Background(), 1), 1, memberID)
	require.NoError(t, err)
	require.Equal(t, int64(20000), acc.BalanceCents)
	require.Equal(t, int64(2000), acc.GiftCents)
}

func TestM4S1_RechargeRegionCap(t *testing.T) {
	r, _, _, _, _, memberRepo := newM4S1Router()
	_, md := doM1(t, r, "POST", "/v1/boss/membership/members", "1", "1", gin.H{"phone_hash": "ph-cap"})
	memberID := idOf(t, md)

	// set regional cap = 10000.
	doM1(t, r, "POST", fmt.Sprintf("/v1/boss/membership/members/%d/region-cap", memberID), "1", "1", gin.H{
		"region_cap_cents": 10000,
	})
	// recharge 20000 > cap → 60003 (errcode.ErrBalanceOver).
	w, _ := doM1(t, r, "POST", fmt.Sprintf("/v1/boss/membership/members/%d/recharge", memberID), "1", "1", gin.H{
		"amount_cents": 20000,
	})
	env := decodeEnvelope(t, w.Body.Bytes())
	require.Equal(t, 60003, env.Code)
	// balance unchanged.
	acc, _ := memberRepo.GetAccount(m3s3TenantCtx(context.Background(), 1), 1, memberID)
	require.Equal(t, int64(0), acc.BalanceCents)
}

func TestM4S1_SubscriptionPaywall(t *testing.T) {
	r, _, _, _, _, _ := newM4S1Router()
	// free tenant → ai_advice denied (pro feature).
	_, cf := doM1(t, r, "POST", "/v1/boss/subscription/check", "1", "1", gin.H{"feature": "ai_advice"})
	require.Equal(t, false, cf["allowed"])
	// subscribe pro.
	_, sub := doM1(t, r, "POST", "/v1/boss/subscription", "1", "1", gin.H{"plan": "pro", "period_months": 1})
	require.Equal(t, "pro", sub["Plan"])
	// ai_advice now allowed.
	_, cf2 := doM1(t, r, "POST", "/v1/boss/subscription/check", "1", "1", gin.H{"feature": "ai_advice"})
	require.Equal(t, true, cf2["allowed"])
}

func TestM4S1_CrossTenantIsolation(t *testing.T) {
	r, _, _, _, _, _ := newM4S1Router()
	onboardActiveSM(t, r, "1", "1")

	// tenant 1 prepay → payment id.
	_, pp := doM1(t, r, "POST", "/v1/boss/payment/prepay", "1", "1", gin.H{
		"subject": "order", "ref_id": 1, "amount_cents": 100,
	})
	payID := idOf(t, payOf(t, pp))

	// tenant 2 GET same payment → 40403 (cross-tenant = not found).
	w, _ := doM1(t, r, "GET", fmt.Sprintf("/v1/boss/payment/payments/%d", payID), "2", "2", nil)
	env := decodeEnvelope(t, w.Body.Bytes())
	require.Equal(t, 40403, env.Code)
}
