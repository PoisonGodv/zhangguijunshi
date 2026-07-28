package payment

// fakewxpay_test.go verifies the deterministic dev implementation of the three
// V3 ports (CONTRACT §15.3.1 / §15.5.8): Apply/Query return stable ids and an
// immediate active status, Prepay is a pure function of the out_trade_no, and
// ParseNotify is a pure JSON decode with no signature verification.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestFakeWxPay_ApplyDeterministic(t *testing.T) {
	f := NewFakeWxPay()
	in := ApplySubMerchantInput{TenantID: 7, StoreID: 3, Provider: ProviderWxPay}
	id1, err := f.Apply(context.Background(), in)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.HasPrefix(id1, "aply_7_") {
		t.Fatalf("applyment id %q want prefix aply_7_", id1)
	}
	// Same inputs in a fresh Fake produce the same first id (counter resets).
	f2 := NewFakeWxPay()
	id2, _ := f2.Apply(context.Background(), in)
	if id1 != id2 {
		t.Fatalf("apply not deterministic across instances: %s vs %s", id1, id2)
	}
	// Second apply within the same Fake advances the counter.
	id3, _ := f.Apply(context.Background(), in)
	if id3 == id1 {
		t.Fatalf("second apply should differ from first: %s", id3)
	}
}

func TestFakeWxPay_QueryReturnsActive(t *testing.T) {
	f := NewFakeWxPay()
	id, _ := f.Apply(context.Background(), ApplySubMerchantInput{TenantID: 1, StoreID: 1, Provider: ProviderWxPay})
	status, subMchID, err := f.Query(context.Background(), 1, id)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != SubMerchantActive {
		t.Fatalf("status = %s, want active", status)
	}
	if subMchID != "submch_"+id {
		t.Fatalf("subMchID = %s, want submch_%s", subMchID, id)
	}
}

func TestFakeWxPay_PrepayDeterministic(t *testing.T) {
	f := NewFakeWxPay()
	in := PrepayInput{OutTradeNo: "trade-xyz", AmountCents: 9900}
	r1, err := f.Prepay(context.Background(), in)
	if err != nil {
		t.Fatalf("prepay: %v", err)
	}
	r2, _ := f.Prepay(context.Background(), in)
	if r1 != r2 {
		t.Fatalf("prepay not deterministic: %+v vs %+v", r1, r2)
	}
	if r1.PrepayID != "prepay_trade-xyz" {
		t.Fatalf("PrepayID = %s", r1.PrepayID)
	}
	if r1.Package != "prepay_id=prepay_trade-xyz" {
		t.Fatalf("Package = %s", r1.Package)
	}
	if r1.SignType != "RSA" {
		t.Fatalf("SignType = %s", r1.SignType)
	}
}

func TestFakeWxPay_ParseNotifySuccess(t *testing.T) {
	f := NewFakeWxPay()
	body := []byte(`{"out_trade_no":"o1","transaction_id":"wx-tx-1","amount_total":9900,"trade_state":"SUCCESS","success_time":"2026-07-28T10:00:00+08:00"}`)
	res, err := f.ParseNotify(context.Background(), nil, body)
	if err != nil {
		t.Fatalf("parse notify: %v", err)
	}
	if res.OutTradeNo != "o1" || res.ProviderTxID != "wx-tx-1" || res.AmountCents != 9900 {
		t.Fatalf("parsed result mismatch: %+v", res)
	}
	if res.Status != PaymentStatusSuccess {
		t.Fatalf("status = %s, want success", res.Status)
	}
	if res.PaidAt.IsZero() {
		t.Fatalf("PaidAt not parsed")
	}
	if string(res.RawPayload) != string(body) {
		t.Fatalf("RawPayload should echo body")
	}
}

func TestFakeWxPay_ParseNotifyFailedState(t *testing.T) {
	f := NewFakeWxPay()
	body := []byte(`{"out_trade_no":"o2","transaction_id":"wx-tx-2","amount_total":100,"trade_state":"PAYERROR"}`)
	res, err := f.ParseNotify(context.Background(), nil, body)
	if err != nil {
		t.Fatalf("parse notify: %v", err)
	}
	if res.Status != PaymentStatusFailed {
		t.Fatalf("status = %s, want failed", res.Status)
	}
}

func TestFakeWxPay_ParseNotifyMalformedBody(t *testing.T) {
	f := NewFakeWxPay()
	_, err := f.ParseNotify(context.Background(), nil, []byte(`{not json`))
	if err == nil {
		t.Fatal("expected error for malformed body")
	}
	// Malformed body is a parse error, NOT a signature error.
	if errors.Is(err, ErrSignInvalid) {
		t.Fatalf("malformed body must not be ErrSignInvalid: %v", err)
	}
}

func TestFakeWxPay_SatisfiesPorts(t *testing.T) {
	// Compile-time assertions already exist in fakewxpay.go; this is a runtime
	// sanity check that one instance serves all three ports.
	var (
		_ SubMerchantApplier = NewFakeWxPay()
		_ Preparer           = NewFakeWxPay()
		_ NotifyParser       = NewFakeWxPay()
	)
}
