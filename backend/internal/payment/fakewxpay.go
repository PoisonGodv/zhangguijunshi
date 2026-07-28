package payment

// fakewxpay.go implements FakeWxPay — the deterministic, never-network
// implementation of all three WeChat Pay V3 anti-corruption ports
// (SubMerchantApplier + Preparer + NotifyParser) for dev and test
// (CONTRACT §15.3.1 / §15.5.8). The main thread wires a single FakeWxPay into
// all three ports of the payment Service.
//
// Determinism rules (§15.5.10): Apply/Prepay outputs are a pure function of
// their inputs (plus a monotonic in-process counter for applyment_id
// uniqueness, whose order is fixed by the test's call sequence); ParseNotify
// is a pure JSON decode of the body with NO signature verification. No
// time.Now / no rand / no network inside any path.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

// FakeWxPay is the dev/test implementation of SubMerchantApplier, Preparer and
// NotifyParser. The zero value is not usable; construct via NewFakeWxPay.
type FakeWxPay struct {
	applySeq int64 // monotonic counter for deterministic applyment_id uniqueness
}

// NewFakeWxPay constructs a FakeWxPay ready to serve all three V3 ports.
func NewFakeWxPay() *FakeWxPay { return &FakeWxPay{} }

// Compile-time assertions that FakeWxPay satisfies the three ports.
var (
	_ SubMerchantApplier = (*FakeWxPay)(nil)
	_ Preparer           = (*FakeWxPay)(nil)
	_ NotifyParser       = (*FakeWxPay)(nil)
)

// Apply returns a deterministic applyment_id immediately (Fake is "即时",
// CONTRACT §15.3.1). It never errors and never touches the network.
func (f *FakeWxPay) Apply(_ context.Context, in ApplySubMerchantInput) (string, error) {
	seq := atomic.AddInt64(&f.applySeq, 1)
	return fmt.Sprintf("aply_%d_%d", in.TenantID, seq), nil
}

// Query returns active + a deterministic sub_mch_id derived from the
// applyment_id. One poll activates the sub-merchant (Fake "即时" transition),
// so Service.QuerySubMerchant writes status=active + SubMchID + ApprovedAt.
func (f *FakeWxPay) Query(_ context.Context, _ int64, applymentID string) (SubMerchantStatus, string, error) {
	return SubMerchantActive, "submch_" + applymentID, nil
}

// Prepay returns a deterministic JSAPI lift-off payload derived purely from
// the input out_trade_no. TimeStamp is a fixed dev constant (the real V3
// client sets a fresh epoch seconds; Fake dev lives outside time).
func (f *FakeWxPay) Prepay(_ context.Context, in PrepayInput) (PrepayResult, error) {
	prepayID := "prepay_" + in.OutTradeNo
	return PrepayResult{
		PrepayID:  prepayID,
		TimeStamp: "1700000000",
		NonceStr:  "nonce_" + in.OutTradeNo,
		Package:   "prepay_id=" + prepayID,
		PaySign:   "paysig_" + in.OutTradeNo,
		SignType:  "RSA",
	}, nil
}

// fakeNotifyBody is the JSON shape FakeWxPay.ParseNotify decodes. It is a
// deliberately flat, documented schema (WeChat V3's real body nests amount and
// uses encrypted resource; the Fake serves dev/test only).
type fakeNotifyBody struct {
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	AmountTotal   int64  `json:"amount_total"`
	TradeState    string `json:"trade_state"`  // SUCCESS | anything-else=failed
	SuccessTime   string `json:"success_time"` // RFC3339; optional for failed
}

// ParseNotify decodes the body JSON into a NotifyResult WITHOUT verifying any
// signature (Fake contract, §15.5.8). A malformed body yields a wrapped
// json error (NOT ErrSignInvalid — that is reserved for signature failure in
// the real parser); HandleNotify propagates the error and leaves the DB
// untouched. RawPayload echoes the raw body for the audit ledger.
func (f *FakeWxPay) ParseNotify(_ context.Context, _ map[string]string, body []byte) (NotifyResult, error) {
	var fb fakeNotifyBody
	if err := json.Unmarshal(body, &fb); err != nil {
		return NotifyResult{}, fmt.Errorf("payment: fake parse notify: %w", err)
	}
	res := NotifyResult{
		OutTradeNo:   fb.OutTradeNo,
		ProviderTxID: fb.TransactionID,
		AmountCents:  fb.AmountTotal,
		Status:       PaymentStatusFailed,
		RawPayload:   body,
	}
	if fb.TradeState == "SUCCESS" {
		res.Status = PaymentStatusSuccess
	}
	if fb.SuccessTime != "" {
		if t, err := time.Parse(time.RFC3339, fb.SuccessTime); err == nil {
			res.PaidAt = t
		}
	}
	return res, nil
}
