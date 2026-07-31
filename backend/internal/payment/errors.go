package payment

// errors.go defines the package's sentinel errors (CONTRACT §15.3.1). A
// cross-tenant read is shaped exactly like a not-found (ARCH-01): the row
// simply is not visible to this tenant, so callers cannot distinguish "absent"
// from "owned by another tenant".

import "errors"

// Sentinel errors. These are the only error identities callers should switch
// on; everything else is a wrapped driver / IO error.
var (
	// ErrNotFound is returned for an absent OR cross-tenant row.
	ErrNotFound = errors.New("payment: not found")

	// ErrSignInvalid is returned by NotifyParser implementations (and the
	// pure VerifyV3Notify helper) when a notify fails signature verification.
	// HandleNotify leaves the DB untouched in this case.
	ErrSignInvalid = errors.New("payment: notify signature invalid")

	// ErrDuplicateOutTradeNo is returned by Prepay / PaymentRepo.Create when
	// out_trade_no already exists. out_trade_no is globally unique across all
	// tenants (migration 000020) because the unauthenticated notify callback
	// resolves the owning tenant from out_trade_no alone; a collision would make
	// that resolution ambiguous.
	ErrDuplicateOutTradeNo = errors.New("payment: duplicate out_trade_no")

	// ErrSubMerchantNotActive is returned by Prepay when the store has no
	// active contracted sub-merchant for the requested provider — without one
	// there is no legal clearer, so no payment may be created.
	ErrSubMerchantNotActive = errors.New("payment: sub-merchant not active")

	// ErrStatusConflict is returned by MarkSuccess when the payment is not in
	// a state that can transition to success (e.g. refunded→success is an
	// illegal reverse transition).
	ErrStatusConflict = errors.New("payment: status conflict")

	// ErrAmountMismatch is returned by HandleNotify when a success notify
	// reports an amount that differs from the payment's recorded AmountCents.
	// The payment is NOT marked success and NOT fulfilled; an audit ledger row
	// with status="amount_mismatch" is written first so the discrepancy is
	// recorded for manual reconciliation (短付/改价攻击防护).
	ErrAmountMismatch = errors.New("payment: notify amount mismatch")

	// ErrAmbiguousOutTradeNo is returned by GetByOutTradeNoAnyTenant when more
	// than one tenant owns the same out_trade_no. out_trade_no is globally
	// unique (migration 000020); a multi-hit means the uniqueness invariant is
	// broken and tenant resolution must fail closed rather than guess.
	ErrAmbiguousOutTradeNo = errors.New("payment: ambiguous out_trade_no across tenants")
)
