package payment

// service.go implements the payment Service (CONTRACT §15.3.1): the orchestrator
// over the three repos and the three WeChat Pay V3 anti-corruption ports. Every
// method resolves the tenant from ctx via multitenancy.EnforceTenant (the single
// chokepoint, ARCH-01) so a cross-tenant read surfaces as ErrNotFound.
//
// The two命门 (critical invariants) live here:
//   - 反二清 (§15.5.3): Prepay refuses to create a payment unless the store has
//     an active sub-merchant — the legal clearer. The Service never holds or
//     moves funds; the ledger is audit-only.
//   - 回调幂等 (§15.5.4): HandleNotify writes the ledger (UNIQUE tx_id dedup),
//     then calls MarkSuccess whose changed flag gates Fulfiller, so a repeat
//     callback writes one ledger row, leaves payment at one success, and
//     fulfils exactly once.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service orchestrates sub-merchant onboarding, payment creation and the
// idempotent notify path.
type Service struct {
	smRepo     SubMerchantRepo
	payRepo    PaymentRepo
	ledgerRepo LedgerRepo
	applier    SubMerchantApplier
	preparer   Preparer
	parser     NotifyParser
	fulfiller  Fulfiller
	now        func() time.Time
	outTradeNo func(tenantID int64, now time.Time) string
}

// Option customises a Service.
type Option func(*Service)

// WithApplier wires the WeChat Pay V3 applyment port (main thread adapts a real
// V3 client or NewFakeWxPay for dev).
func WithApplier(a SubMerchantApplier) Option { return func(s *Service) { s.applier = a } }

// WithPreparer wires the unified-order port.
func WithPreparer(p Preparer) Option { return func(s *Service) { s.preparer = p } }

// WithNotifyParser wires the notify verify/parse port.
func WithNotifyParser(p NotifyParser) Option { return func(s *Service) { s.parser = p } }

// WithFulfiller wires the fulfilment port (nil-safe in HandleNotify).
func WithFulfiller(f Fulfiller) Option { return func(s *Service) { s.fulfiller = f } }

// WithNowFunc overrides the service clock (deterministic notify timestamps /
// applied_at in tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// WithOutTradeNoFunc overrides out_trade_no generation (deterministic in tests).
func WithOutTradeNoFunc(f func(tenantID int64, now time.Time) string) Option {
	return func(s *Service) {
		if f != nil {
			s.outTradeNo = f
		}
	}
}

// NewService constructs a payment Service. now defaults to time.Now and
// outTradeNo to a tenant+nanosestamp string; both are overridable via Options
// so tests are deterministic.
func NewService(smRepo SubMerchantRepo, payRepo PaymentRepo, ledgerRepo LedgerRepo, opts ...Option) *Service {
	s := &Service{
		smRepo:     smRepo,
		payRepo:    payRepo,
		ledgerRepo: ledgerRepo,
		now:        time.Now,
		outTradeNo: func(tenantID int64, now time.Time) string {
			return fmt.Sprintf("zg_%d_%d", tenantID, now.UnixNano())
		},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// resolveStoreID returns in's store when > 0, else the store carried in ctx
// (matches the marketing / billing convention).
func resolveStoreID(ctx context.Context, storeID int64) int64 {
	if storeID > 0 {
		return storeID
	}
	if mc, ok := multitenancy.FromContext(ctx); ok {
		return mc.StoreID
	}
	return 0
}

// maskLast masks all but the last `keep` runes of s (desensitisation for the
// materials snapshot). Shorter strings are fully masked.
func maskLast(s string, keep int) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= keep {
		return strings.Repeat("*", len(runes))
	}
	return strings.Repeat("*", len(runes)-keep) + string(runes[len(runes)-keep:])
}

// materialsSnapshot builds the desensitised onboarding materials map stored on
// the sub-merchant record (JSONB in production).
func materialsSnapshot(in ApplySubMerchantInput) map[string]any {
	return map[string]any{
		"business_name": in.BusinessName,
		"license_no":    maskLast(in.LicenseNo, 4),
		"license_type":  in.LicenseType,
		"contact_name":  in.ContactName,
		"contact_phone": maskLast(in.ContactPhone, 3),
		"provider":      in.Provider,
	}
}

// ApplySubMerchant submits a sub-merchant onboarding application. It writes the
// record (status=pending) and, when an applier is wired, records the returned
// applyment_id for later QuerySubMerchant polls. Per ARCH-01 the tenant comes
// from ctx, not from the input.
func (s *Service) ApplySubMerchant(ctx context.Context, in ApplySubMerchantInput) (*SubMerchant, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	sm := &SubMerchant{
		TenantID:  tenantID,
		StoreID:   resolveStoreID(ctx, in.StoreID),
		Provider:  in.Provider,
		Status:    SubMerchantPending,
		RateBPS:   in.RateBPS,
		Materials: materialsSnapshot(in),
		AppliedAt: now,
	}
	if s.applier != nil {
		applymentID, err := s.applier.Apply(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("payment: apply sub-merchant: %w", err)
		}
		sm.ApplymentID = applymentID
	}
	return s.smRepo.Create(ctx, sm)
}

// QuerySubMerchant polls the applyment status and writes it through. On a
// pending→active transition it sets SubMchID + ApprovedAt. With no applier
// wired it is a plain read. Cross-tenant → ErrNotFound.
func (s *Service) QuerySubMerchant(ctx context.Context, id int64) (*SubMerchant, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	sm, err := s.smRepo.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if sm.ApplymentID == "" || s.applier == nil {
		return sm, nil
	}
	status, subMchID, err := s.applier.Query(ctx, tenantID, sm.ApplymentID)
	if err != nil {
		return nil, fmt.Errorf("payment: query applyment: %w", err)
	}
	if status != sm.Status || subMchID != sm.SubMchID {
		var approvedAt time.Time
		if status == SubMerchantActive && sm.ApprovedAt == nil {
			approvedAt = s.now()
		}
		if err := s.smRepo.UpdateStatus(ctx, tenantID, id, status, subMchID, approvedAt); err != nil {
			return nil, err
		}
		sm, err = s.smRepo.GetByID(ctx, tenantID, id)
		if err != nil {
			return nil, err
		}
	}
	return sm, nil
}

// ListSubMerchants lists the ctx tenant's sub-merchants.
func (s *Service) ListSubMerchants(ctx context.Context) ([]*SubMerchant, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.smRepo.ListByTenant(ctx, tenantID)
}

// Prepay creates a pending payment and calls unified-order. It requires an
// active sub-merchant for (store, provider) — without a clearer no payment may
// be created (反二清) — and rejects a duplicate out_trade_no. out_trade_no is
// generated when the caller omits it. A nil preparer still creates the payment
// (result nil) so the creation/uniqueness path is testable without a port.
func (s *Service) Prepay(ctx context.Context, in PrepayInput) (*Payment, *PrepayResult, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, nil, err
	}
	storeID := resolveStoreID(ctx, in.StoreID)
	// This sprint is wxpay-only (CONTRACT §15.1: alipay ISV is deferred to
	// M4-S2; FakeWxPay implements only wxpay). PrepayInput carries no Provider
	// field (signature frozen), so the provider is fixed here.
	provider := ProviderWxPay

	// 反二清: there must be a legal clearer (active sub-merchant).
	sm, err := s.smRepo.GetActiveByStore(ctx, tenantID, storeID, provider)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, ErrSubMerchantNotActive
		}
		return nil, nil, err
	}

	outTradeNo := in.OutTradeNo
	if outTradeNo == "" {
		outTradeNo = s.outTradeNo(tenantID, s.now())
	}
	// out_trade_no UNIQUE per tenant.
	if existing, err := s.payRepo.GetByOutTradeNo(ctx, tenantID, outTradeNo); err == nil {
		_ = existing
		return nil, nil, ErrDuplicateOutTradeNo
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, err
	}

	now := s.now()
	p := &Payment{
		TenantID:      tenantID,
		StoreID:       storeID,
		SubMerchantID: sm.ID,
		OutTradeNo:    outTradeNo,
		Provider:      provider,
		Subject:       in.Subject,
		RefID:         in.RefID,
		AmountCents:   in.AmountCents,
		Type:          PaymentTypePay,
		Status:        PaymentStatusPending,
		CreatedAt:     now,
	}
	p, err = s.payRepo.Create(ctx, p)
	if err != nil {
		return nil, nil, err
	}

	var result *PrepayResult
	if s.preparer != nil {
		in.TenantID = tenantID
		in.StoreID = storeID
		in.SubMerchantID = sm.ID
		in.OutTradeNo = outTradeNo
		res, err := s.preparer.Prepay(ctx, in)
		if err != nil {
			return nil, nil, fmt.Errorf("payment: prepay: %w", err)
		}
		result = &res
	}
	return p, result, nil
}

// GetPayment loads a payment by id (cross-tenant → ErrNotFound).
func (s *Service) GetPayment(ctx context.Context, id int64) (*Payment, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.payRepo.GetByID(ctx, tenantID, id)
}

// HandleNotify is the idempotent WeChat notify entry point (§15.5.4 命门):
//  1. ParseNotify verifies/decodes (failure → return err, DB untouched).
//  2. Resolve tenant + find the payment by out_trade_no (cross-tenant →
//     ErrNotFound; ledger untouched).
//  3. ledgerRepo.Append writes the audit row — UNIQUE(tenant,provider,tx_id)
//     dedupes so a repeat callback yields one row.
//  4. MarkSuccess performs the pending→success transition and reports changed.
//  5. On a genuine transition (changed) with a non-nil Fulfiller, fulfil once.
//     Fulfilment failure is best-effort (logged, swallowed) — the payment is
//     already success and must not roll back.
//
// Returns the final payment.
func (s *Service) HandleNotify(ctx context.Context, headers map[string]string, body []byte) (*Payment, error) {
	if s.parser == nil {
		return nil, errors.New("payment: notify parser not configured")
	}
	// 1. verify + parse (no tenant needed; failure → 不动库).
	res, err := s.parser.ParseNotify(ctx, headers, body)
	if err != nil {
		return nil, err
	}
	// 2. tenant + locate payment.
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	p, err := s.payRepo.GetByOutTradeNo(ctx, tenantID, res.OutTradeNo)
	if err != nil {
		return nil, err
	}
	// 3. ledger audit (idempotent by provider_tx_id).
	ledgerStatus := "success"
	if res.Status != PaymentStatusSuccess {
		ledgerStatus = "failed"
	}
	var orderID int64
	if p.Subject == SubjectOrder {
		orderID = p.RefID
	}
	ledger := &PaymentLedger{
		TenantID:      tenantID,
		StoreID:       p.StoreID,
		SubMerchantID: p.SubMerchantID,
		OrderID:       orderID,
		Provider:      p.Provider,
		ProviderTxID:  res.ProviderTxID,
		AmountCents:   res.AmountCents,
		FeeCents:      0,
		Type:          p.Type,
		Status:        ledgerStatus,
		RawPayload:    res.RawPayload,
		CreatedAt:     s.now(),
	}
	if !res.PaidAt.IsZero() {
		paidAt := res.PaidAt
		ledger.PaidAt = &paidAt
	}
	if _, err := s.ledgerRepo.Append(ctx, ledger); err != nil {
		return nil, err
	}
	// 4/5. state machine + fulfil (idempotent).
	if res.Status == PaymentStatusSuccess {
		p, changed, err := s.payRepo.MarkSuccess(ctx, tenantID, p.ID, res.ProviderTxID, res.PaidAt)
		if err != nil {
			return nil, err
		}
		if changed && s.fulfiller != nil {
			_ = s.fulfiller.Fulfill(ctx, p) // best-effort; never roll back payment
		}
		return p, nil
	}
	// failed notify: best-effort pending→failed; do not fulfil.
	_ = s.payRepo.UpdateStatus(ctx, tenantID, p.ID, PaymentStatusFailed)
	cur, err := s.payRepo.GetByID(ctx, tenantID, p.ID)
	if err != nil {
		return nil, err
	}
	return cur, nil
}

// ListLedger reads the ctx tenant's ledger rows (boss reconciliation view),
// newest first, up to limit.
func (s *Service) ListLedger(ctx context.Context, limit int) ([]*PaymentLedger, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.ledgerRepo.ListByTenant(ctx, tenantID, limit)
}

// ResolveTenantByNotifyBody recovers the owning tenant from a raw WeChat notify
// body. The notify callback is unauthenticated (CONTRACT §15.7) so the request
// context carries no tenant; the tenant is resolved from the payment matched by
// out_trade_no (unique per tenant). The HTTP handler calls this, injects the
// tenant via multitenancy.WithContext, then calls HandleNotify — which then
// enforces tenant as usual. ErrNotFound when no payment matches the out_trade_no.
func (s *Service) ResolveTenantByNotifyBody(ctx context.Context, body []byte) (int64, error) {
	if s.parser == nil {
		return 0, errors.New("payment: notify parser not configured")
	}
	res, err := s.parser.ParseNotify(ctx, nil, body)
	if err != nil {
		return 0, err
	}
	p, err := s.payRepo.GetByOutTradeNoAnyTenant(ctx, res.OutTradeNo)
	if err != nil {
		return 0, err
	}
	return p.TenantID, nil
}
