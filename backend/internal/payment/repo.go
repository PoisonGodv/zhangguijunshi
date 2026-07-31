package payment

// repo.go declares the three persistence port interfaces (CONTRACT §15.3.1):
// SubMerchantRepo, PaymentRepo and LedgerRepo. Each has a Memory
// implementation (dev/test, sync.Mutex-guarded, -race safe) and a PG
// implementation (production, pgxmock-tested) — the dual-implementation rule
// of §15.5.7. Tenant isolation is structural: every read/write is scoped by
// tenantID, so a row stored under tenant A is invisible under tenant B.

import (
	"context"
	"time"
)

// SubMerchantRepo persists sub-merchant onboarding records.
type SubMerchantRepo interface {
	Create(ctx context.Context, sm *SubMerchant) (*SubMerchant, error)
	GetByID(ctx context.Context, tenantID, id int64) (*SubMerchant, error)
	// GetActiveByStore returns the status=active sub-merchant for
	// (tenantID, storeID, provider), or ErrNotFound when none — Prepay uses
	// this to enforce that a clearer exists before creating a payment.
	GetActiveByStore(ctx context.Context, tenantID, storeID int64, provider string) (*SubMerchant, error)
	// UpdateStatus advances onboarding state, filling SubMchID/ApprovedAt on
	// success. approvedAt zero-value leaves ApprovedAt unset.
	UpdateStatus(ctx context.Context, tenantID, id int64, status SubMerchantStatus, subMchID string, approvedAt time.Time) error
	ListByTenant(ctx context.Context, tenantID int64) ([]*SubMerchant, error)
}

// PaymentRepo persists payment orders and drives the pending→success state
// machine.
type PaymentRepo interface {
	Create(ctx context.Context, p *Payment) (*Payment, error)
	GetByID(ctx context.Context, tenantID, id int64) (*Payment, error)
	GetByOutTradeNo(ctx context.Context, tenantID int64, outTradeNo string) (*Payment, error)
	// GetByOutTradeNoAnyTenant loads a payment by out_trade_no WITHOUT tenant
	// scoping. It exists for the unauthenticated WeChat notify callback (which
	// carries no tenant) to recover the payment's owning tenant from the
	// out_trade_no alone (CONTRACT §15.7). Used ONLY by ResolveTenantByNotifyBody.
	GetByOutTradeNoAnyTenant(ctx context.Context, outTradeNo string) (*Payment, error)
	// MarkSuccess is the idempotent pending→success transition:
	//   - pending → success: fill ProviderTxID/PaidAt/UpdatedAt, return (p, true, nil)
	//   - success → success: no-op, return (p, false, nil)  (repeat callback)
	//   - other   → return (nil, false, ErrStatusConflict)
	// Cross-tenant (or absent) → (nil, false, ErrNotFound).
	MarkSuccess(ctx context.Context, tenantID, id int64, providerTxID string, paidAt time.Time) (p *Payment, changed bool, err error)
	// MarkFailed is the guarded pending→failed transition used by the failed
	// notify path. It flips status ONLY when the payment is still pending:
	//   - pending → failed: return (true, nil)
	//   - any other state (success/failed/refunded), or absent/cross-tenant:
	//     no-op, return (false, nil)
	// so a late failed callback can never revert an already-succeeded payment.
	// Absent and not-pending are intentionally indistinguishable (PG implements
	// this as a single conditional UPDATE whose RowsAffected is 0 in both cases;
	// the Memory impl mirrors that). The caller has already tenant-scoped the
	// read via GetByOutTradeNo before invoking MarkFailed.
	MarkFailed(ctx context.Context, tenantID, id int64) (changed bool, err error)
	UpdateStatus(ctx context.Context, tenantID, id int64, status PaymentStatus) error
}

// LedgerRepo persists the anti-secondary-clearing audit ledger.
type LedgerRepo interface {
	// Append is the idempotent ledger write. UNIQUE(tenant_id, provider,
	// provider_tx_id) means a repeat callback with the same provider_tx_id
	// returns the already-stored row (nil error) instead of duplicating.
	// provider_tx_id empty → write directly (no dedup). Returns the written
	// (or already-stored) row.
	Append(ctx context.Context, l *PaymentLedger) (*PaymentLedger, error)
	ListByTenant(ctx context.Context, tenantID int64, limit int) ([]*PaymentLedger, error)
}
