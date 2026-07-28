// Package payment implements the WeChat Pay service-provider flow for M4-S1
// (CONTRACT §15): sub-merchant onboarding (sub_merchants), payment orders
// (payments) and the anti-secondary-clearing audit ledger (payment_ledger).
//
// The package owns NO funds: capital is cleared directly by WeChat Pay to the
// contracted sub-merchant (payments.sub_merchant_id marks the clearer) and the
// ledger is audit-only — there is no "platform balance" concept anywhere in
// this package (反二清, CONTRACT §15.5.3).
//
// WeChat Pay V3 is modelled as three self-declared anti-corruption ports —
// SubMerchantApplier (onboarding), Preparer (unified order) and NotifyParser
// (notify verify) — plus a deterministic FakeWxPay for dev/test that never
// touches the network. V3 request signing and notify verification are pure
// stdlib-crypto functions (SignV3Authorization / VerifyV3Notify); no
// third-party WeChat SDK is imported (§15.5.8).
//
// Notify idempotency (§15.5.4) is enforced two ways: the ledger has
// UNIQUE(tenant_id, provider, provider_tx_id) so a repeat callback writes one
// row, and payments follow a pending→success state machine whose MarkSuccess
// reports a changed flag so a repeat callback does not re-run Fulfiller.
package payment
