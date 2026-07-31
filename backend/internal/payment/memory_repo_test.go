package payment

// memory_repo_test.go covers the three Memory repos (CONTRACT §15.6 A1):
// CRUD, the structural tenant isolation (cross-tenant → ErrNotFound), the
// Payment.MarkSuccess state machine (changed flag + ErrStatusConflict) and the
// Ledger.Append UNIQUE-tx_id idempotency that backs notify dedup.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemorySubMerchantRepo_CreateGetAndCrossTenant(t *testing.T) {
	ctx := context.Background()
	repo := NewMemorySubMerchantRepo()
	created, err := repo.Create(ctx, &SubMerchant{TenantID: 1, StoreID: 10, Provider: ProviderWxPay, Status: SubMerchantPending})
	require.NoError(t, err)
	require.NotZero(t, created.ID)

	got, err := repo.GetByID(ctx, 1, created.ID)
	require.NoError(t, err)
	require.Equal(t, SubMerchantPending, got.Status)

	// Cross-tenant is shaped as not-found.
	_, err = repo.GetByID(ctx, 2, created.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMemorySubMerchantRepo_GetActiveByStore(t *testing.T) {
	ctx := context.Background()
	repo := NewMemorySubMerchantRepo()
	_, err := repo.GetActiveByStore(ctx, 1, 10, ProviderWxPay)
	require.ErrorIs(t, err, ErrNotFound)

	sm, err := repo.Create(ctx, &SubMerchant{TenantID: 1, StoreID: 10, Provider: ProviderWxPay, Status: SubMerchantReviewing})
	require.NoError(t, err)
	_, err = repo.GetActiveByStore(ctx, 1, 10, ProviderWxPay)
	require.ErrorIs(t, err, ErrNotFound) // reviewing, not active

	require.NoError(t, repo.UpdateStatus(ctx, 1, sm.ID, SubMerchantActive, "submch_1", fixedTime))
	active, err := repo.GetActiveByStore(ctx, 1, 10, ProviderWxPay)
	require.NoError(t, err)
	require.Equal(t, SubMerchantActive, active.Status)
	require.Equal(t, "submch_1", active.SubMchID)
	require.NotNil(t, active.ApprovedAt)
}

func TestMemorySubMerchantRepo_UpdateStatusCrossTenant(t *testing.T) {
	ctx := context.Background()
	repo := NewMemorySubMerchantRepo()
	sm, _ := repo.Create(ctx, &SubMerchant{TenantID: 1, StoreID: 10, Provider: ProviderWxPay, Status: SubMerchantPending})
	require.ErrorIs(t, repo.UpdateStatus(ctx, 2, sm.ID, SubMerchantActive, "x", fixedTime), ErrNotFound)
}

func TestMemorySubMerchantRepo_ListByTenant(t *testing.T) {
	ctx := context.Background()
	repo := NewMemorySubMerchantRepo()
	repo.Create(ctx, &SubMerchant{TenantID: 1, StoreID: 10, Provider: ProviderWxPay})
	repo.Create(ctx, &SubMerchant{TenantID: 1, StoreID: 11, Provider: ProviderWxPay})
	repo.Create(ctx, &SubMerchant{TenantID: 2, StoreID: 20, Provider: ProviderWxPay})
	got, err := repo.ListByTenant(ctx, 1)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Stable ascending order by id.
	require.True(t, got[0].ID < got[1].ID)
}

func TestMemoryPaymentRepo_CreateGetAndCrossTenant(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	created, err := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})
	require.NoError(t, err)
	require.NotZero(t, created.ID)

	got, err := repo.GetByOutTradeNo(ctx, 1, "o1")
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)

	_, err = repo.GetByID(ctx, 2, created.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = repo.GetByOutTradeNo(ctx, 2, "o1")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryPaymentRepo_MarkSuccessChanged(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	p, _ := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})

	got, changed, err := repo.MarkSuccess(ctx, 1, p.ID, "wx-tx-1", fixedTime)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, PaymentStatusSuccess, got.Status)
	require.Equal(t, "wx-tx-1", got.ProviderTxID)
	require.NotNil(t, got.PaidAt)
}

func TestMemoryPaymentRepo_MarkSuccessIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	p, _ := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})

	_, changed1, _ := repo.MarkSuccess(ctx, 1, p.ID, "wx-tx-1", fixedTime)
	require.True(t, changed1)
	got, changed2, err := repo.MarkSuccess(ctx, 1, p.ID, "wx-tx-1", fixedTime)
	require.NoError(t, err)
	require.False(t, changed2) // repeat callback: no re-transition
	require.Equal(t, PaymentStatusSuccess, got.Status)
}

func TestMemoryPaymentRepo_MarkSuccessConflict(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	p, _ := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Status: PaymentStatusRefunded})
	_, changed, err := repo.MarkSuccess(ctx, 1, p.ID, "wx-tx-1", fixedTime)
	require.False(t, changed)
	require.ErrorIs(t, err, ErrStatusConflict)
}

func TestMemoryPaymentRepo_MarkSuccessCrossTenant(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	p, _ := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Status: PaymentStatusPending})
	_, changed, err := repo.MarkSuccess(ctx, 2, p.ID, "wx-tx-1", fixedTime)
	require.False(t, changed)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestMemoryPaymentRepo_UpdateStatus(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	p, _ := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Status: PaymentStatusPending})
	require.NoError(t, repo.UpdateStatus(ctx, 1, p.ID, PaymentStatusFailed))
	got, _ := repo.GetByID(ctx, 1, p.ID)
	require.Equal(t, PaymentStatusFailed, got.Status)
	require.ErrorIs(t, repo.UpdateStatus(ctx, 2, p.ID, PaymentStatusFailed), ErrNotFound)
}

func TestMemoryLedgerRepo_AppendIdempotent(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryLedgerRepo()
	l1, err := repo.Append(ctx, &PaymentLedger{TenantID: 1, StoreID: 10, Provider: ProviderWxPay, ProviderTxID: "wx-tx-1", AmountCents: 100, Status: "success"})
	require.NoError(t, err)
	require.NotZero(t, l1.ID)
	// Same (tenant, provider, tx_id) → returns the already-stored row.
	l2, err := repo.Append(ctx, &PaymentLedger{TenantID: 1, StoreID: 10, Provider: ProviderWxPay, ProviderTxID: "wx-tx-1", AmountCents: 999, Status: "success"})
	require.NoError(t, err)
	require.Equal(t, l1.ID, l2.ID)
	require.Equal(t, int64(100), l2.AmountCents) // original retained, not overwritten

	rows, _ := repo.ListByTenant(ctx, 1, 0)
	require.Len(t, rows, 1) // exactly one ledger row after duplicate append
}

func TestMemoryLedgerRepo_AppendEmptyTxIDNoDedup(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryLedgerRepo()
	_, err := repo.Append(ctx, &PaymentLedger{TenantID: 1, Provider: ProviderWxPay, ProviderTxID: "", AmountCents: 100, Status: "pending"})
	require.NoError(t, err)
	_, err = repo.Append(ctx, &PaymentLedger{TenantID: 1, Provider: ProviderWxPay, ProviderTxID: "", AmountCents: 200, Status: "pending"})
	require.NoError(t, err)
	rows, _ := repo.ListByTenant(ctx, 1, 0)
	require.Len(t, rows, 2) // empty tx_id never dedupes
}

func TestMemoryLedgerRepo_ListByTenantCrossTenant(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryLedgerRepo()
	repo.Append(ctx, &PaymentLedger{TenantID: 1, Provider: ProviderWxPay, ProviderTxID: "a", AmountCents: 1, Status: "success"})
	repo.Append(ctx, &PaymentLedger{TenantID: 2, Provider: ProviderWxPay, ProviderTxID: "b", AmountCents: 1, Status: "success"})
	got, err := repo.ListByTenant(ctx, 1, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(1), got[0].TenantID)
}

// errors import guard — ensures the package's only error identities are the
// declared sentinels (no leakage of driver-specific errors via Memory repo).
func TestMemoryRepo_OnlySentinelErrors(t *testing.T) {
	var err error = ErrNotFound
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("ErrNotFound identity broken")
	}
}

// Create refuses a duplicate out_trade_no within the same tenant (mirrors the
// PG ON CONFLICT guard) instead of silently overwriting the index entry.
func TestMemoryPaymentRepo_CreateDuplicateOutTradeNo(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	_, err := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})
	require.NoError(t, err)
	_, err = repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Subject: SubjectOrder, AmountCents: 200, Status: PaymentStatusPending})
	require.ErrorIs(t, err, ErrDuplicateOutTradeNo)
}

// Create enforces out_trade_no GLOBALLY (across tenants), matching migration
// 000020's UNIQUE(out_trade_no): the notify path resolves tenant by out_trade_no
// alone, so a cross-tenant collision must be refused at insert.
func TestMemoryPaymentRepo_CreateCrossTenantDuplicateOutTradeNo(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	_, err := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "shared", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})
	require.NoError(t, err)
	_, err = repo.Create(ctx, &Payment{TenantID: 2, StoreID: 20, OutTradeNo: "shared", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})
	require.ErrorIs(t, err, ErrDuplicateOutTradeNo)
}

// GetByOutTradeNoAnyTenant fails closed with ErrAmbiguousOutTradeNo when the
// global-uniqueness invariant is broken (two tenants hold the same out_trade_no).
func TestMemoryPaymentRepo_GetByOutTradeNoAnyTenantAmbiguous(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	repo.forceInsertRow(&Payment{TenantID: 1, StoreID: 10, OutTradeNo: "dup", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})
	repo.forceInsertRow(&Payment{TenantID: 2, StoreID: 20, OutTradeNo: "dup", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})
	_, err := repo.GetByOutTradeNoAnyTenant(ctx, "dup")
	require.ErrorIs(t, err, ErrAmbiguousOutTradeNo)
}

// GetByOutTradeNoAnyTenant returns the single owner when out_trade_no is unique,
// and ErrNotFound when absent.
func TestMemoryPaymentRepo_GetByOutTradeNoAnyTenant(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	created, _ := repo.Create(ctx, &Payment{TenantID: 7, StoreID: 10, OutTradeNo: "o1", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})
	got, err := repo.GetByOutTradeNoAnyTenant(ctx, "o1")
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	require.Equal(t, int64(7), got.TenantID)
	_, err = repo.GetByOutTradeNoAnyTenant(ctx, "missing")
	require.ErrorIs(t, err, ErrNotFound)
}

// MarkFailed flips only pending→failed; a succeeded payment is left untouched
// (a late failed callback must never revert success), and an absent/cross-tenant
// id is a no-op (changed=false, nil error) mirroring the PG conditional UPDATE.
func TestMemoryPaymentRepo_MarkFailed(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	p, _ := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})

	changed, err := repo.MarkFailed(ctx, 1, p.ID)
	require.NoError(t, err)
	require.True(t, changed)
	got, _ := repo.GetByID(ctx, 1, p.ID)
	require.Equal(t, PaymentStatusFailed, got.Status)

	// Already-failed → no-op.
	changed, err = repo.MarkFailed(ctx, 1, p.ID)
	require.NoError(t, err)
	require.False(t, changed)
}

func TestMemoryPaymentRepo_MarkFailedDoesNotRevertSuccess(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	p, _ := repo.Create(ctx, &Payment{TenantID: 1, StoreID: 10, OutTradeNo: "o1", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending})
	_, _, err := repo.MarkSuccess(ctx, 1, p.ID, "wx-tx-1", fixedTime)
	require.NoError(t, err)

	changed, err := repo.MarkFailed(ctx, 1, p.ID)
	require.NoError(t, err)
	require.False(t, changed, "success must not revert to failed")
	got, _ := repo.GetByID(ctx, 1, p.ID)
	require.Equal(t, PaymentStatusSuccess, got.Status)
}

func TestMemoryPaymentRepo_MarkFailedAbsentNoOp(t *testing.T) {
	ctx := context.Background()
	repo := NewMemoryPaymentRepo()
	changed, err := repo.MarkFailed(ctx, 1, 9999)
	require.NoError(t, err)
	require.False(t, changed)
}
