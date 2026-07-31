package payment

// service_test.go covers the Service orchestration (CONTRACT §15.6 A1),
// including the two命门: 反二清 (Prepay requires an active sub-merchant) and
// 回调幂等 (HandleNotify — one ledger row, one payment, one fulfilment after a
// duplicate callback). Cross-tenant reads surface as ErrNotFound; a
// verification failure (ErrSignInvalid) leaves the DB untouched; Fulfiller is
// nil-safe.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newSvc builds a Service wired with FakeWxPay on all three ports, a fixed
// clock and a deterministic out_trade_no generator. Extra opts (e.g.
// WithFulfiller / a stub parser) are applied after the defaults so they win.
func newSvc(t *testing.T, opts ...Option) (*Service, *MemorySubMerchantRepo, *MemoryPaymentRepo, *MemoryLedgerRepo) {
	t.Helper()
	sm := NewMemorySubMerchantRepo()
	pay := NewMemoryPaymentRepo()
	led := NewMemoryLedgerRepo()
	def := []Option{
		WithApplier(NewFakeWxPay()),
		WithPreparer(NewFakeWxPay()),
		WithNotifyParser(NewFakeWxPay()),
		WithNowFunc(func() time.Time { return fixedTime }),
		WithOutTradeNoFunc(func(tid int64, _ time.Time) string { return fmt.Sprintf("o_%d", tid) }),
	}
	return NewService(sm, pay, led, append(def, opts...)...), sm, pay, led
}

// activateSubMerchant applies + queries a sub-merchant so it reaches active
// status (FakeWxPay's query is "即时" active), enabling Prepay.
func activateSubMerchant(t *testing.T, svc *Service, ctx context.Context, storeID int64) *SubMerchant {
	t.Helper()
	applied, err := svc.ApplySubMerchant(ctx, ApplySubMerchantInput{
		StoreID: storeID, Provider: ProviderWxPay, BusinessName: "小店",
		LicenseNo: "91110000MA00ABCDEF", ContactPhone: "13800001111",
	})
	require.NoError(t, err)
	queried, err := svc.QuerySubMerchant(ctx, applied.ID)
	require.NoError(t, err)
	require.Equal(t, SubMerchantActive, queried.Status, "FakeWxPay query should activate")
	return queried
}

// countingFulfiller counts Fulfill invocations (thread-safe) and remembers the
// last payment it saw.
type countingFulfiller struct {
	mu   sync.Mutex
	n    int
	last *Payment
}

func (f *countingFulfiller) Fulfill(_ context.Context, p *Payment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.last = p
	return nil
}

func (f *countingFulfiller) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// errParser is a NotifyParser stub that always fails with ErrSignInvalid.
type errParser struct{}

func (errParser) ParseNotify(_ context.Context, _ map[string]string, _ []byte) (NotifyResult, error) {
	return NotifyResult{}, ErrSignInvalid
}

func TestService_ApplySubMerchant_WritesRecordAndApplymentID(t *testing.T) {
	svc, sm, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	applied, err := svc.ApplySubMerchant(ctx, ApplySubMerchantInput{
		StoreID: 10, Provider: ProviderWxPay, BusinessName: "B", LicenseNo: "L12345678",
		ContactPhone: "13900001234", RateBPS: 38,
	})
	require.NoError(t, err)
	require.NotEmpty(t, applied.ApplymentID, "FakeWxPay should return an applyment id")
	require.Equal(t, SubMerchantPending, applied.Status)
	require.Equal(t, int64(1), applied.TenantID)

	got, _ := sm.GetByID(ctx, 1, applied.ID)
	require.Equal(t, applied.ApplymentID, got.ApplymentID)
	// Materials snapshot is desensitised (license "L12345678" → 5 stars + last 4;
	// phone "13900001234" → 8 stars + last 3).
	require.Equal(t, "*****5678", got.Materials["license_no"])
	require.Equal(t, "********234", got.Materials["contact_phone"])
}

func TestService_QuerySubMerchant_Activates(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	applied, _ := svc.ApplySubMerchant(ctx, ApplySubMerchantInput{StoreID: 10, Provider: ProviderWxPay})
	queried, err := svc.QuerySubMerchant(ctx, applied.ID)
	require.NoError(t, err)
	require.Equal(t, SubMerchantActive, queried.Status)
	require.NotEmpty(t, queried.SubMchID)
	require.NotNil(t, queried.ApprovedAt)
}

func TestService_ListSubMerchants_CrossTenant(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	svc.ApplySubMerchant(tenantCtx(1, 10), ApplySubMerchantInput{StoreID: 10, Provider: ProviderWxPay})
	svc.ApplySubMerchant(tenantCtx(2, 20), ApplySubMerchantInput{StoreID: 20, Provider: ProviderWxPay})
	got1, err := svc.ListSubMerchants(tenantCtx(1, 10))
	require.NoError(t, err)
	require.Len(t, got1, 1)
	require.Equal(t, int64(1), got1[0].TenantID)
}

func TestService_Prepay_RequiresActiveSubMerchant(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	// No sub-merchant yet → 反二清 guard rejects.
	_, _, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 7, AmountCents: 9900, OutTradeNo: "o1"})
	require.ErrorIs(t, err, ErrSubMerchantNotActive)
}

func TestService_Prepay_SuccessReturnsPaymentAndResult(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	p, res, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 77, AmountCents: 9900, OutTradeNo: "o1"})
	require.NoError(t, err)
	require.Equal(t, PaymentStatusPending, p.Status)
	require.NotZero(t, p.SubMerchantID, "payment must reference the clearer sub-merchant")
	require.NotNil(t, res, "PrepayResult must be returned when preparer is wired")
	require.Equal(t, "prepay_o1", res.PrepayID)
}

func TestService_Prepay_DuplicateOutTradeNo(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	_, _, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 77, AmountCents: 9900, OutTradeNo: "o1"})
	require.NoError(t, err)
	_, _, err = svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 77, AmountCents: 9900, OutTradeNo: "o1"})
	require.ErrorIs(t, err, ErrDuplicateOutTradeNo)
}

func TestService_Prepay_GeneratesOutTradeWhenOmitted(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	p, _, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100})
	require.NoError(t, err)
	require.Equal(t, "o_1", p.OutTradeNo, "generator should derive from tenant id")
}

func TestService_GetPayment_CrossTenant(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	p, _, _ := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "o1"})
	_, err := svc.GetPayment(tenantCtx(2, 20), p.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestService_HandleNotify_Idempotent(t *testing.T) {
	ff := &countingFulfiller{}
	svc, _, pay, led := newSvc(t, WithFulfiller(ff))
	_ = led
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	p, _, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 77, AmountCents: 9900, OutTradeNo: "o1"})
	require.NoError(t, err)
	require.Equal(t, PaymentStatusPending, p.Status)

	body := fakeBody("o1", "wx-tx-1", 9900, true)

	// First callback: pending→success, ledger row created, fulfil once.
	p1, err := svc.HandleNotify(ctx, nil, body)
	require.NoError(t, err)
	require.Equal(t, PaymentStatusSuccess, p1.Status)
	require.Equal(t, "wx-tx-1", p1.ProviderTxID)
	require.Equal(t, 1, ff.count())

	// Second callback with the SAME provider_tx_id: idempotent — no new ledger
	// row, no re-transition, no re-fulfil.
	p2, err := svc.HandleNotify(ctx, nil, body)
	require.NoError(t, err)
	require.Equal(t, PaymentStatusSuccess, p2.Status)

	ledgerRows, _ := svc.ListLedger(ctx, 0)
	require.Len(t, ledgerRows, 1, "ledger must hold exactly one row after duplicate notify")
	require.Equal(t, 1, ff.count(), "fulfiller must not run again on duplicate notify")

	// Payment is still a single success row.
	got, _ := pay.GetByID(ctx, 1, p.ID)
	require.Equal(t, PaymentStatusSuccess, got.Status)
}

func TestService_HandleNotify_FulfillerNilSafe(t *testing.T) {
	// No WithFulfiller: HandleNotify must still succeed and not panic.
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "o1"})
	p, err := svc.HandleNotify(ctx, nil, fakeBody("o1", "wx-tx-9", 100, true))
	require.NoError(t, err)
	require.Equal(t, PaymentStatusSuccess, p.Status)
}

func TestService_HandleNotify_SignInvalidLeavesDBUntouched(t *testing.T) {
	svc, _, pay, _ := newSvc(t, WithNotifyParser(errParser{}))
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	p, _, _ := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "o1"})

	_, err := svc.HandleNotify(ctx, nil, fakeBody("o1", "wx-tx-1", 100, true))
	require.ErrorIs(t, err, ErrSignInvalid)

	// Nothing persisted: ledger empty, payment still pending.
	ledgerRows, _ := svc.ListLedger(ctx, 0)
	require.Empty(t, ledgerRows)
	got, _ := pay.GetByID(ctx, 1, p.ID)
	require.Equal(t, PaymentStatusPending, got.Status)
}

func TestService_HandleNotify_CrossTenantNotFound(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx1 := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx1, 10)
	svc.Prepay(ctx1, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "o1"})

	// Same out_trade_no under tenant 2 → not found, no ledger pollution.
	ctx2 := tenantCtx(2, 20)
	_, err := svc.HandleNotify(ctx2, nil, fakeBody("o1", "wx-tx-1", 100, true))
	require.ErrorIs(t, err, ErrNotFound)

	rows2, _ := svc.ListLedger(ctx2, 0)
	require.Empty(t, rows2, "tenant 2 ledger must be empty")
	rows1, _ := svc.ListLedger(ctx1, 0)
	require.Empty(t, rows1, "tenant 1 ledger must remain empty (no match)")
}

func TestService_HandleNotify_NotFoundForUnknownOutTradeNo(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	_, err := svc.HandleNotify(ctx, nil, fakeBody("missing", "wx-tx-x", 100, true))
	require.ErrorIs(t, err, ErrNotFound)
}

func TestService_ListLedger_NewestFirst(t *testing.T) {
	svc, _, _, led := newSvc(t)
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	// Two distinct payments → two ledger rows.
	for i, tx := range []string{"tx-a", "tx-b"} {
		otn := "o" + tx
		svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectRecharge, RefID: int64(i), AmountCents: 100, OutTradeNo: otn})
		svc.HandleNotify(ctx, nil, fakeBody(otn, tx, 100, true))
	}
	got, err := svc.ListLedger(ctx, 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Ordering is newest-first (created_at desc); both share fixedTime so id desc.
	require.True(t, got[0].ID > got[1].ID)
	_ = led
}

func TestService_RejectsNoTenantContext(t *testing.T) {
	// A bare context.Background() has no tenant → every method must refuse.
	svc, _, _, _ := newSvc(t)
	_, err := svc.ApplySubMerchant(context.Background(), ApplySubMerchantInput{StoreID: 10, Provider: ProviderWxPay})
	require.Error(t, err)
	_, err = svc.ListSubMerchants(context.Background())
	require.Error(t, err)
	_, err = svc.GetPayment(context.Background(), 1)
	require.Error(t, err)
	_, err = svc.HandleNotify(context.Background(), nil, fakeBody("o1", "x", 1, true))
	require.Error(t, err)
}

func TestService_FulfillerFailureDoesNotRollback(t *testing.T) {
	// A failing Fulfiller must NOT cause HandleNotify to error or un-mark the
	// payment (best-effort fulfilment, §15.3.1).
	svc, _, pay, _ := newSvc(t, WithFulfiller(failingFulfiller{}))
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "o1"})
	p, err := svc.HandleNotify(ctx, nil, fakeBody("o1", "wx-tx-1", 100, true))
	require.NoError(t, err, "fulfilment failure is best-effort")
	require.Equal(t, PaymentStatusSuccess, p.Status)
	got, _ := pay.GetByID(ctx, 1, p.ID)
	require.Equal(t, PaymentStatusSuccess, got.Status, "payment stays success despite fulfil failure")
}

// failingFulfiller always returns an error.
type failingFulfiller struct{}

func (failingFulfiller) Fulfill(context.Context, *Payment) error {
	return errors.New("downstream fulfilment broken")
}

// ===== Regression tests for the 5 confirmed payment bugs =====

// Bug #2: a success notify whose amount differs from the payment's recorded
// AmountCents must be rejected (短付防护) — no MarkSuccess, no fulfil, but an
// audit ledger row with status="amount_mismatch" is written.
func TestService_HandleNotify_AmountMismatchBlocked(t *testing.T) {
	ff := &countingFulfiller{}
	svc, _, pay, _ := newSvc(t, WithFulfiller(ff))
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	p, _, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 77, AmountCents: 9900, OutTradeNo: "o1"})
	require.NoError(t, err)

	// Notify claims only 100 cents against a 9900-cent order → short-pay.
	_, err = svc.HandleNotify(ctx, nil, fakeBody("o1", "wx-tx-short", 100, true))
	require.ErrorIs(t, err, ErrAmountMismatch)

	// Payment must remain pending (NOT marked success) and NOT fulfilled.
	got, _ := pay.GetByID(ctx, 1, p.ID)
	require.Equal(t, PaymentStatusPending, got.Status, "short-pay must not mark success")
	require.Empty(t, got.ProviderTxID, "short-pay must not record the provider tx")
	require.Equal(t, 0, ff.count(), "short-pay must not fulfil")

	// Exactly one audit row, flagged amount_mismatch, carrying the paid amount.
	rows, _ := svc.ListLedger(ctx, 0)
	require.Len(t, rows, 1)
	require.Equal(t, "amount_mismatch", rows[0].Status)
	require.Equal(t, int64(100), rows[0].AmountCents)
}

// Bug #3: a late failed callback must NOT revert an already-succeeded payment
// (MarkFailed is guarded on status='pending').
func TestService_HandleNotify_FailedAfterSuccessDoesNotRevert(t *testing.T) {
	svc, _, pay, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	p, _, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "o1"})
	require.NoError(t, err)

	// First: a genuine success notify.
	p1, err := svc.HandleNotify(ctx, nil, fakeBody("o1", "wx-tx-ok", 100, true))
	require.NoError(t, err)
	require.Equal(t, PaymentStatusSuccess, p1.Status)

	// Then: a late failed notify for the same order (different tx id). It must
	// NOT flip the payment back to failed.
	p2, err := svc.HandleNotify(ctx, nil, fakeBody("o1", "wx-tx-late-fail", 100, false))
	require.NoError(t, err)
	require.Equal(t, PaymentStatusSuccess, p2.Status, "failed callback must not revert success")

	got, _ := pay.GetByID(ctx, 1, p.ID)
	require.Equal(t, PaymentStatusSuccess, got.Status)
}

// Bug #5: concurrent Prepay with the same out_trade_no must admit exactly one
// (the repo Create is the authoritative guard; the pre-check alone is racy).
func TestService_Prepay_ConcurrentSameOutTradeNo(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var okCount, dupCount int
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "race"})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okCount++
			case errors.Is(err, ErrDuplicateOutTradeNo):
				dupCount++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, okCount, "exactly one concurrent Prepay may win")
	require.Equal(t, workers-1, dupCount, "all losers must see ErrDuplicateOutTradeNo")
}

// Bug #4: out_trade_no is globally unique — a second tenant reusing another
// tenant's out_trade_no is rejected at Prepay.
func TestService_Prepay_CrossTenantDuplicateOutTradeNo(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx1 := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx1, 10)
	_, _, err := svc.Prepay(ctx1, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "shared"})
	require.NoError(t, err)

	ctx2 := tenantCtx(2, 20)
	activateSubMerchant(t, svc, ctx2, 20)
	_, _, err = svc.Prepay(ctx2, PrepayInput{StoreID: 20, Subject: SubjectOrder, RefID: 2, AmountCents: 100, OutTradeNo: "shared"})
	require.ErrorIs(t, err, ErrDuplicateOutTradeNo, "out_trade_no must be globally unique across tenants")
}

// Bug #4: when the global-uniqueness invariant is broken (two tenants hold the
// same out_trade_no), tenant resolution for the unauthenticated notify must
// fail closed with ErrAmbiguousOutTradeNo rather than guess.
func TestService_ResolveTenantByNotifyBody_AmbiguousFailsClosed(t *testing.T) {
	svc, _, pay, _ := newSvc(t)
	// Manufacture the cross-tenant collision that production's UNIQUE forbids.
	pay.forceInsertRow(&Payment{TenantID: 1, StoreID: 10, OutTradeNo: "dup", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending, Provider: ProviderWxPay, Type: PaymentTypePay})
	pay.forceInsertRow(&Payment{TenantID: 2, StoreID: 20, OutTradeNo: "dup", Subject: SubjectOrder, AmountCents: 100, Status: PaymentStatusPending, Provider: ProviderWxPay, Type: PaymentTypePay})

	_, err := svc.ResolveTenantByNotifyBody(context.Background(), nil, fakeBody("dup", "wx-tx-1", 100, true))
	require.ErrorIs(t, err, ErrAmbiguousOutTradeNo, "ambiguous out_trade_no must fail closed")
}

// Bug #4 (happy path): a globally-unique out_trade_no resolves to exactly its
// owning tenant.
func TestService_ResolveTenantByNotifyBody_ResolvesOwner(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	ctx1 := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx1, 10)
	_, _, err := svc.Prepay(ctx1, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "o1"})
	require.NoError(t, err)

	tid, err := svc.ResolveTenantByNotifyBody(context.Background(), nil, fakeBody("o1", "wx-tx-1", 100, true))
	require.NoError(t, err)
	require.Equal(t, int64(1), tid)
}

// Bug #1: the headers map is threaded into the parser. A header-recording
// parser proves HandleNotify / ResolveTenantByNotifyBody forward the headers
// they are given (the production V3 parser needs them to verify the signature).
type headerRecordingParser struct {
	mu      sync.Mutex
	lastHdr map[string]string
	inner   NotifyParser
}

func (p *headerRecordingParser) ParseNotify(ctx context.Context, headers map[string]string, body []byte) (NotifyResult, error) {
	p.mu.Lock()
	p.lastHdr = headers
	p.mu.Unlock()
	return p.inner.ParseNotify(ctx, headers, body)
}

func (p *headerRecordingParser) seen() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastHdr
}

func TestService_NotifyForwardsHeadersToParser(t *testing.T) {
	rec := &headerRecordingParser{inner: NewFakeWxPay()}
	svc, _, _, _ := newSvc(t, WithNotifyParser(rec))
	ctx := tenantCtx(1, 10)
	activateSubMerchant(t, svc, ctx, 10)
	_, _, err := svc.Prepay(ctx, PrepayInput{StoreID: 10, Subject: SubjectOrder, RefID: 1, AmountCents: 100, OutTradeNo: "o1"})
	require.NoError(t, err)

	hdr := map[string]string{
		"Wechatpay-Timestamp": "1700000000",
		"Wechatpay-Nonce":     "n",
		"Wechatpay-Serial":    "s",
		"Wechatpay-Signature": "sig",
	}
	_, err = svc.HandleNotify(ctx, hdr, fakeBody("o1", "wx-tx-1", 100, true))
	require.NoError(t, err)
	require.Equal(t, hdr, rec.seen(), "HandleNotify must forward the Wechatpay-* headers to the parser")
}
