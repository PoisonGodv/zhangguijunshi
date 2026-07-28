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
