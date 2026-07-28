package payment

// model.go defines the persisted domain types for the payment package
// (CONTRACT §15.3.1): SubMerchant (onboarding record), Payment (order
// state machine) and PaymentLedger (反二清 audit row), plus their status /
// type constants. Field names, types and ordering match the migration DDL in
// §15.2 one-for-one.

import "time"

// Provider constants (sub_merchants.provider / payments.provider).
const (
	ProviderWxPay  = "wxpay"
	ProviderAlipay = "alipay"
)

// SubMerchantStatus is the onboarding state of a contracted sub-merchant
// (sub_merchants.status). pending→reviewing→active is the happy path;
// rejected/frozen are terminal/risk states.
type SubMerchantStatus string

const (
	SubMerchantPending   SubMerchantStatus = "pending"
	SubMerchantReviewing SubMerchantStatus = "reviewing"
	SubMerchantActive    SubMerchantStatus = "active"
	SubMerchantRejected  SubMerchantStatus = "rejected"
	SubMerchantFrozen    SubMerchantStatus = "frozen"
)

// SubMerchant is a service-provider onboarding record (sub_merchants table).
// SubMchID is empty until WeChat approves the application; it is the
// anti-secondary-clearing clearer for payments created against this store.
type SubMerchant struct {
	ID, TenantID, StoreID int64
	Provider              string // wxpay|alipay
	SubMchID              string // 受理成功后的特约商户号(空=未成功)
	ApplymentID           string // 微信申请单号
	Status                SubMerchantStatus
	RateBPS               int32          // 费率基点 38=0.38%
	Materials             map[string]any // 进件材料快照(脱敏, JSONB)
	AppliedAt             time.Time
	ApprovedAt            *time.Time
	UpdatedAt             time.Time
}

// PaymentStatus is the payment order state machine (payments.status).
// pending→success is the only forward success transition; it is irreversible
// (repeat callbacks are idempotent, not re-transitions).
type PaymentStatus string

const (
	PaymentStatusPending  PaymentStatus = "pending"
	PaymentStatusSuccess  PaymentStatus = "success"
	PaymentStatusFailed   PaymentStatus = "failed"
	PaymentStatusRefunded PaymentStatus = "refunded"
)

// PaymentType distinguishes pay vs refund rows (payments.type /
// payment_ledger.type). Refund flow is reserved (M4-S2); this sprint only
// emits pay.
type PaymentType string

const (
	PaymentTypePay    PaymentType = "pay"
	PaymentTypeRefund PaymentType = "refund"
)

// Subject constants (payments.subject) are the fulfilment dispatch key: on a
// pending→success transition the service's Fulfiller is invoked and the main
// thread's adapter switches on Subject to route to billing / membership /
// subscription. See CONTRACT §15.3.1.
const (
	SubjectOrder        = "order"        // → billing.MarkOrderPaid (RefID=orderID)
	SubjectRecharge     = "recharge"     // → membership.Recharge (RefID=memberID)
	SubjectSubscription = "subscription" // → subscription.ConfirmPayment
)

// Payment is one business-side payment order (payments table). The state
// machine is pending→success; ProviderTxID/PaidAt are filled by the notify
// path. sub_merchant_id marks the clearer (反二清): funds settle to the
// contracted sub-merchant, never to the platform.
type Payment struct {
	ID, TenantID, StoreID, SubMerchantID int64
	OutTradeNo                           string // 商户订单号 (UNIQUE per tenant)
	Provider                             string // wxpay|alipay
	ProviderTxID                         string // 微信交易号 (回调填, 对账键)
	Subject                              string // order|recharge|subscription
	RefID                                int64  // 按 Subject 解释
	AmountCents                          int64
	Type                                 PaymentType
	Status                               PaymentStatus
	PaidAt                               *time.Time
	CreatedAt                            time.Time
	UpdatedAt                            time.Time
}

// PaymentLedger is one audit row (payment_ledger table, 禁软删). The ledger is
// the anti-secondary-clearing audit trail: it records what was paid to which
// sub-merchant but holds no balance. UNIQUE(tenant_id, provider,
// provider_tx_id) makes a repeat WeChat callback write exactly one row.
type PaymentLedger struct {
	ID, TenantID, StoreID, SubMerchantID int64
	OrderID                              int64 // 可 0 (非订单支付)
	Provider                             string
	ProviderTxID                         string // UNIQUE(tenant,provider,tx_id) 幂等核心
	AmountCents                          int64
	FeeCents                             int64
	Type                                 PaymentType
	Status                               string // success|pending|failed|refunded
	RawPayload                           []byte // 回调原始报文 (审计, JSONB 序列化)
	PaidAt                               *time.Time
	CreatedAt                            time.Time
}
