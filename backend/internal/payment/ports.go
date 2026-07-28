package payment

// ports.go declares the four self-declared anti-corruption ports (CONTRACT
// §15.3.1) that decouple this package from WeChat Pay V3 and from the
// downstream fulfilment domains (billing / membership / subscription). The
// payment package never imports those domains; the main thread supplies
// concrete adapters at wiring time.
//
//   - SubMerchantApplier / Preparer / NotifyParser model the WeChat Pay V3
//     service-provider SDK (applyment / prepay / notify). FakeWxPay is the
//     deterministic dev implementation; the real V3 HTTP client is deferred
//     to production (§15.5.8) but the V3 signing primitives in sign.go are
//     real and tested.
//   - Fulfiller is invoked by HandleNotify only on a genuine pending→success
//     transition, so a repeat callback never re-fulfils.

import (
	"context"
	"time"
)

// ApplySubMerchantInput is the onboarding request carried into
// SubMerchantApplier.Apply and Service.ApplySubMerchant.
type ApplySubMerchantInput struct {
	TenantID, StoreID int64
	Provider          string // wxpay
	BusinessName      string
	LicenseNo         string // 营业执照号
	LicenseType       string // individual|enterprise
	ContactName       string
	ContactPhone      string
	RateBPS           int32
}

// SubMerchantApplier is the WeChat Pay V3 applyment port. The real
// implementation calls api.mch.weixin.qq.com; FakeWxPay returns a deterministic
// applyment_id immediately (dev/test). Service.ApplySubMerchant writes the
// record and stores the returned applyment_id for later QuerySubMerchant polls.
type SubMerchantApplier interface {
	Apply(ctx context.Context, in ApplySubMerchantInput) (applymentID string, err error)
	Query(ctx context.Context, tenantID int64, applymentID string) (status SubMerchantStatus, subMchID string, err error)
}

// PrepayInput is the unified-order request. SubMerchantID is authoritative
// after Service.Prepay resolves the store's active sub-merchant (the service
// overwrites it before calling Preparer so a stale/missing input value cannot
// route payment to the wrong clearer). OpenID empty means Native (扫码) instead
// of JSAPI.
type PrepayInput struct {
	TenantID, StoreID, SubMerchantID int64
	OutTradeNo                       string
	Subject                          string
	RefID                            int64
	AmountCents                      int64
	OpenID                           string // 付款方 openid (JSAPI; 空=Native 扫码)
	Description                      string
}

// PrepayResult is the unified-order response — the JSAPI lift-off parameters
// the mini-program signs and invokes wx.requestPayment with.
type PrepayResult struct {
	PrepayID  string
	TimeStamp string
	NonceStr  string
	Package   string // "prepay_id=xxx"
	PaySign   string
	SignType  string // RSA
}

// Preparer is the WeChat Pay V3 prepay port. FakeWxPay returns a deterministic
// PrepayResult derived from the input (no network).
type Preparer interface {
	Prepay(ctx context.Context, in PrepayInput) (PrepayResult, error)
}

// NotifyResult is the normalised, signature-verified outcome of a WeChat
// notify. Status is success|failed; PaidAt is the upstream trade time.
type NotifyResult struct {
	OutTradeNo   string
	ProviderTxID string
	AmountCents  int64
	Status       PaymentStatus // success|failed
	PaidAt       time.Time
	RawPayload   []byte
}

// NotifyParser is the WeChat Pay V3 notify port. A real implementation
// verifies the V3 signature (calling VerifyV3Notify) and AES-GCM-decrypts the
// resource; FakeWxPay skips verification and just JSON-decodes the body
// (dev/test). Verification failure MUST be reported as ErrSignInvalid so
// HandleNotify knows to leave the DB untouched.
type NotifyParser interface {
	ParseNotify(ctx context.Context, headers map[string]string, body []byte) (NotifyResult, error)
}

// Fulfiller is the fulfilment port invoked after a genuine pending→success
// transition. The main thread supplies a composite adapter that switches on
// Payment.Subject to call billing.MarkOrderPaid / membership.Recharge /
// subscription.ConfirmPayment. A nil Fulfiller means "do not fulfil" (safe for
// dev/test where no downstream exists); HandleNotify tolerates nil.
type Fulfiller interface {
	Fulfill(ctx context.Context, p *Payment) error
}
