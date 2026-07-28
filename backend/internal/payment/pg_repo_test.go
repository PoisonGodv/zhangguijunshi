package payment

// pg_repo_test.go exercises the three PG repos against pgxmock (CONTRACT
// §15.6 A1: "INSERT...ON CONFLICT upsert + SELECT"). The pool uses
// QueryMatcherEqual because the SQL text contains regex metacharacters; tests
// are same-package so they reference the unexported sql* constants. The core
// idempotency of PGLedgerRepo.Append (ON CONFLICT DO NOTHING → fallback SELECT)
// is the load-bearing assertion mirrored from the Memory repo.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

func smColumns() []string {
	return []string{"id", "tenant_id", "store_id", "provider", "sub_mch_id", "applyment_id", "status", "rate_bps", "materials", "applied_at", "approved_at", "updated_at"}
}

func paymentColumns() []string {
	return []string{"id", "tenant_id", "store_id", "sub_merchant_id", "out_trade_no", "provider", "provider_tx_id", "subject", "ref_id", "amount_cents", "type", "status", "paid_at", "created_at", "updated_at"}
}

func ledgerColumns() []string {
	return []string{"id", "tenant_id", "store_id", "sub_merchant_id", "order_id", "provider", "provider_tx_id", "amount_cents", "fee_cents", "type", "status", "raw_payload", "paid_at", "created_at"}
}

func TestPGSubMerchantRepo_CreateAndGet(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSubMerchantRepo(mock)

	mock.ExpectQuery(sqlSubMerchantCreate).
		WithArgs(int64(1), int64(10), ProviderWxPay, "aply_1", SubMerchantPending, int32(38), map[string]any(nil), fixedTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(42)))
	sm, err := repo.Create(context.Background(), &SubMerchant{
		TenantID: 1, StoreID: 10, Provider: ProviderWxPay, ApplymentID: "aply_1",
		Status: SubMerchantPending, RateBPS: 38, AppliedAt: fixedTime,
	})
	require.NoError(t, err)
	require.Equal(t, int64(42), sm.ID)

	mock.ExpectQuery(sqlSubMerchantGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(smColumns()).AddRow(
			int64(42), int64(1), int64(10), ProviderWxPay, "submch_1", "aply_1", "active", int32(38),
			[]byte(`{"license_no":"****1234"}`), fixedTime, fixedTime, fixedTime,
		))
	got, err := repo.GetByID(context.Background(), 1, 42)
	require.NoError(t, err)
	require.Equal(t, SubMerchantActive, got.Status)
	require.Equal(t, "submch_1", got.SubMchID)
	require.Equal(t, "****1234", got.Materials["license_no"])
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGSubMerchantRepo_GetByIDNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSubMerchantRepo(mock)
	mock.ExpectQuery(sqlSubMerchantGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetByID(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGSubMerchantRepo_UpdateStatus(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGSubMerchantRepo(mock)
	// nullTime is the package-internal helper used by UpdateStatus; the test
	// mirrors it exactly so pgxmock's value matcher accepts the arg.
	mock.ExpectExec(sqlSubMerchantUpdateStatus).
		WithArgs(SubMerchantActive, "submch_1", nullTime(fixedTime), int64(1), int64(42)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, repo.UpdateStatus(context.Background(), 1, 42, SubMerchantActive, "submch_1", fixedTime))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGPaymentRepo_Create(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGPaymentRepo(mock)
	mock.ExpectQuery(sqlPaymentCreate).
		WithArgs(int64(1), int64(10), int64(42), "o1", ProviderWxPay, "", SubjectOrder, int64(100), int64(7), PaymentTypePay, PaymentStatusPending).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(5)))
	p, err := repo.Create(context.Background(), &Payment{
		TenantID: 1, StoreID: 10, SubMerchantID: 42, OutTradeNo: "o1", Provider: ProviderWxPay,
		Subject: SubjectOrder, RefID: 100, AmountCents: 7, Type: PaymentTypePay, Status: PaymentStatusPending,
	})
	require.NoError(t, err)
	require.Equal(t, int64(5), p.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGPaymentRepo_MarkSuccessChanged(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGPaymentRepo(mock)
	// Step 1: load current row (pending).
	mock.ExpectQuery(sqlPaymentGetForMark).
		WithArgs(int64(1), int64(5)).
		WillReturnRows(pgxmock.NewRows(paymentColumns()).AddRow(
			int64(5), int64(1), int64(10), int64(42), "o1", ProviderWxPay, "", SubjectOrder,
			int64(100), int64(7), "pay", "pending", nil, fixedTime, fixedTime,
		))
	// Step 2: conditional UPDATE returns the success row.
	mock.ExpectQuery(sqlPaymentMarkSuccess).
		WithArgs("wx-tx-1", fixedTime, int64(1), int64(5)).
		WillReturnRows(pgxmock.NewRows(paymentColumns()).AddRow(
			int64(5), int64(1), int64(10), int64(42), "o1", ProviderWxPay, "wx-tx-1", SubjectOrder,
			int64(100), int64(7), "pay", "success", fixedTime, fixedTime, fixedTime,
		))
	p, changed, err := repo.MarkSuccess(context.Background(), 1, 5, "wx-tx-1", fixedTime)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, PaymentStatusSuccess, p.Status)
	require.Equal(t, "wx-tx-1", p.ProviderTxID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGPaymentRepo_MarkSuccessIdempotent(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGPaymentRepo(mock)
	// Load returns already-success → no UPDATE issued.
	mock.ExpectQuery(sqlPaymentGetForMark).
		WithArgs(int64(1), int64(5)).
		WillReturnRows(pgxmock.NewRows(paymentColumns()).AddRow(
			int64(5), int64(1), int64(10), int64(42), "o1", ProviderWxPay, "wx-tx-1", SubjectOrder,
			int64(100), int64(7), "pay", "success", fixedTime, fixedTime, fixedTime,
		))
	p, changed, err := repo.MarkSuccess(context.Background(), 1, 5, "wx-tx-1", fixedTime)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, PaymentStatusSuccess, p.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGPaymentRepo_MarkSuccessConflict(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGPaymentRepo(mock)
	mock.ExpectQuery(sqlPaymentGetForMark).
		WithArgs(int64(1), int64(5)).
		WillReturnRows(pgxmock.NewRows(paymentColumns()).AddRow(
			int64(5), int64(1), int64(10), int64(42), "o1", ProviderWxPay, "wx-tx-1", SubjectOrder,
			int64(100), int64(7), "pay", "refunded", fixedTime, fixedTime, fixedTime,
		))
	_, changed, err := repo.MarkSuccess(context.Background(), 1, 5, "wx-tx-1", fixedTime)
	require.False(t, changed)
	require.ErrorIs(t, err, ErrStatusConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGLedgerRepo_AppendInsert(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGLedgerRepo(mock)
	mock.ExpectQuery(sqlLedgerAppend).
		WithArgs(int64(1), int64(10), int64(42), int64(100), ProviderWxPay, "wx-tx-1", int64(9900), int64(0), PaymentTypePay, "success", []byte(`{"k":"v"}`), (*time.Time)(nil)).
		WillReturnRows(pgxmock.NewRows(ledgerColumns()).AddRow(
			int64(9), int64(1), int64(10), int64(42), int64(100), ProviderWxPay, "wx-tx-1", int64(9900), int64(0),
			"pay", "success", []byte(`{"k":"v"}`), fixedTime, fixedTime,
		))
	l, err := repo.Append(context.Background(), &PaymentLedger{
		TenantID: 1, StoreID: 10, SubMerchantID: 42, OrderID: 100, Provider: ProviderWxPay,
		ProviderTxID: "wx-tx-1", AmountCents: 9900, FeeCents: 0, Type: PaymentTypePay, Status: "success",
		RawPayload: []byte(`{"k":"v"}`),
	})
	require.NoError(t, err)
	require.Equal(t, int64(9), l.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGLedgerRepo_AppendDuplicateReturnsExisting(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGLedgerRepo(mock)
	// INSERT ... ON CONFLICT DO NOTHING RETURNING yields no rows (conflict).
	mock.ExpectQuery(sqlLedgerAppend).
		WithArgs(int64(1), int64(10), int64(42), int64(100), ProviderWxPay, "wx-tx-1", int64(9900), int64(0), PaymentTypePay, "success", []byte(`{"k":"v"}`), (*time.Time)(nil)).
		WillReturnError(pgx.ErrNoRows)
	// Fallback SELECT by (tenant, provider, tx_id) returns the stored row.
	mock.ExpectQuery(sqlLedgerGetByTx).
		WithArgs(int64(1), ProviderWxPay, "wx-tx-1").
		WillReturnRows(pgxmock.NewRows(ledgerColumns()).AddRow(
			int64(9), int64(1), int64(10), int64(42), int64(100), ProviderWxPay, "wx-tx-1", int64(9900), int64(0),
			"pay", "success", []byte(`{"k":"v"}`), fixedTime, fixedTime,
		))
	l, err := repo.Append(context.Background(), &PaymentLedger{
		TenantID: 1, StoreID: 10, SubMerchantID: 42, OrderID: 100, Provider: ProviderWxPay,
		ProviderTxID: "wx-tx-1", AmountCents: 9900, Type: PaymentTypePay, Status: "success",
		RawPayload: []byte(`{"k":"v"}`),
	})
	require.NoError(t, err)
	require.Equal(t, int64(9), l.ID) // existing row, not a new insert
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGLedgerRepo_ListByTenant(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGLedgerRepo(mock)
	mock.ExpectQuery(sqlLedgerList).
		WithArgs(int64(1), 50).
		WillReturnRows(pgxmock.NewRows(ledgerColumns()).
			AddRow(int64(1), int64(1), int64(10), int64(42), int64(100), ProviderWxPay, "wx-tx-1", int64(9900), int64(0), "pay", "success", []byte(`{}`), fixedTime, fixedTime))
	got, err := repo.ListByTenant(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "wx-tx-1", got[0].ProviderTxID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	// Nil pools degrade to ErrNotFound / empty rather than panicking.
	smRepo := NewPGSubMerchantRepo(nil)
	_, err := smRepo.GetByID(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)

	payRepo := NewPGPaymentRepo(nil)
	_, _, err = payRepo.MarkSuccess(context.Background(), 1, 1, "x", fixedTime)
	require.ErrorIs(t, err, ErrNotFound)

	ledgerRepo := NewPGLedgerRepo(nil)
	out, err := ledgerRepo.ListByTenant(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Empty(t, out)

	// Append with nil pool returns the input row (no-op), not an error.
	l, err := ledgerRepo.Append(context.Background(), &PaymentLedger{TenantID: 1, ProviderTxID: "x"})
	require.NoError(t, err)
	require.NotNil(t, l)
}
