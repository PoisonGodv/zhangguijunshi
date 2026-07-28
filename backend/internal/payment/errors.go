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

	// ErrDuplicateOutTradeNo is returned by Prepay when out_trade_no already
	// exists for the tenant (UNIQUE(tenant_id, out_trade_no)).
	ErrDuplicateOutTradeNo = errors.New("payment: duplicate out_trade_no")

	// ErrSubMerchantNotActive is returned by Prepay when the store has no
	// active contracted sub-merchant for the requested provider — without one
	// there is no legal clearer, so no payment may be created.
	ErrSubMerchantNotActive = errors.New("payment: sub-merchant not active")

	// ErrStatusConflict is returned by MarkSuccess when the payment is not in
	// a state that can transition to success (e.g. refunded→success is an
	// illegal reverse transition).
	ErrStatusConflict = errors.New("payment: status conflict")
)
