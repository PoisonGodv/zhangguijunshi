package payment

// memory_repo.go implements the three Memory repos (CONTRACT §15.3.1 /
// §15.5.7): MemorySubMerchantRepo, MemoryPaymentRepo, MemoryLedgerRepo. Each
// keeps rows in a map keyed by id (plus auxiliary uniqueness indexes), guarded
// by a sync.Mutex so concurrent Service calls are -race safe. IDs come from an
// atomic counter (deterministic per call order). Reads return defensive copies
// so a caller can never mutate a stored row in place.
//
// Tenant isolation is structural: every read filters by tenantID, so a row
// stored under tenant A is invisible under tenant B and surfaces as
// ErrNotFound — identical to a genuinely absent row (ARCH-01).

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// cloneSubMerchant returns a defensive copy of sm (including a fresh materials
// map so callers cannot mutate the stored snapshot).
func cloneSubMerchant(sm *SubMerchant) *SubMerchant {
	if sm == nil {
		return nil
	}
	cp := *sm
	if sm.Materials != nil {
		cp.Materials = make(map[string]any, len(sm.Materials))
		for k, v := range sm.Materials {
			cp.Materials[k] = v
		}
	}
	return &cp
}

// clonePayment returns a defensive copy of p.
func clonePayment(p *Payment) *Payment {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// cloneLedger returns a defensive copy of l (including a fresh RawPayload
// slice so callers cannot mutate the stored audit bytes).
func cloneLedger(l *PaymentLedger) *PaymentLedger {
	if l == nil {
		return nil
	}
	cp := *l
	if l.RawPayload != nil {
		cp.RawPayload = append([]byte(nil), l.RawPayload...)
	}
	return &cp
}

// ===== SubMerchant =====

// MemorySubMerchantRepo is the in-memory SubMerchantRepo (dev/test).
type MemorySubMerchantRepo struct {
	mu   sync.Mutex
	seq  int64
	rows map[int64]*SubMerchant
}

// NewMemorySubMerchantRepo constructs an empty MemorySubMerchantRepo.
func NewMemorySubMerchantRepo() *MemorySubMerchantRepo {
	return &MemorySubMerchantRepo{rows: make(map[int64]*SubMerchant)}
}

// Create allocates an id + timestamps and stores sm. It returns a copy.
func (r *MemorySubMerchantRepo) Create(_ context.Context, sm *SubMerchant) (*SubMerchant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := atomic.AddInt64(&r.seq, 1)
	now := time.Now()
	sm.ID = id
	if sm.AppliedAt.IsZero() {
		sm.AppliedAt = now
	}
	sm.UpdatedAt = now
	stored := cloneSubMerchant(sm)
	r.rows[id] = stored
	return cloneSubMerchant(stored), nil
}

// GetByID returns the sub-merchant for (tenantID, id), or ErrNotFound when
// absent or cross-tenant.
func (r *MemorySubMerchantRepo) GetByID(_ context.Context, tenantID, id int64) (*SubMerchant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sm, ok := r.rows[id]
	if !ok || sm.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneSubMerchant(sm), nil
}

// GetActiveByStore returns the status=active sub-merchant for
// (tenantID, storeID, provider), or ErrNotFound. If multiple are active the
// most recently applied one wins (stable enough for dev; production enforces
// uniqueness via the uniq_submch_provider_mchid index).
func (r *MemorySubMerchantRepo) GetActiveByStore(_ context.Context, tenantID, storeID int64, provider string) (*SubMerchant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *SubMerchant
	for _, sm := range r.rows {
		if sm.TenantID == tenantID && sm.StoreID == storeID && sm.Provider == provider && sm.Status == SubMerchantActive {
			if best == nil || sm.AppliedAt.After(best.AppliedAt) {
				best = sm
			}
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return cloneSubMerchant(best), nil
}

// UpdateStatus advances the onboarding state. approvedAt non-zero sets
// ApprovedAt (used when transitioning to active); zero leaves it unchanged.
func (r *MemorySubMerchantRepo) UpdateStatus(_ context.Context, tenantID, id int64, status SubMerchantStatus, subMchID string, approvedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sm, ok := r.rows[id]
	if !ok || sm.TenantID != tenantID {
		return ErrNotFound
	}
	sm.Status = status
	if subMchID != "" {
		sm.SubMchID = subMchID
	}
	if !approvedAt.IsZero() {
		ac := approvedAt
		sm.ApprovedAt = &ac
	}
	sm.UpdatedAt = time.Now()
	return nil
}

// ListByTenant returns all sub-merchants for tenantID ordered by id for
// deterministic output.
func (r *MemorySubMerchantRepo) ListByTenant(_ context.Context, tenantID int64) ([]*SubMerchant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*SubMerchant, 0)
	for _, sm := range r.rows {
		if sm.TenantID == tenantID {
			out = append(out, cloneSubMerchant(sm))
		}
	}
	// Stable order by id.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// ===== Payment =====

// MemoryPaymentRepo is the in-memory PaymentRepo (dev/test). It carries a
// per-tenant out_trade_no uniqueness index so Prepay can detect duplicates
// without a full scan.
type MemoryPaymentRepo struct {
	mu      sync.Mutex
	seq     int64
	rows    map[int64]*Payment
	byTrade map[int64]map[string]int64 // tenantID -> outTradeNo -> payment id
}

// NewMemoryPaymentRepo constructs an empty MemoryPaymentRepo.
func NewMemoryPaymentRepo() *MemoryPaymentRepo {
	return &MemoryPaymentRepo{
		rows:    make(map[int64]*Payment),
		byTrade: make(map[int64]map[string]int64),
	}
}

// Create allocates an id + timestamps and registers the out_trade_no in the
// per-tenant uniqueness index. It returns a copy.
func (r *MemoryPaymentRepo) Create(_ context.Context, p *Payment) (*Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := atomic.AddInt64(&r.seq, 1)
	now := time.Now()
	p.ID = id
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	stored := clonePayment(p)
	r.rows[id] = stored
	if p.OutTradeNo != "" {
		if r.byTrade[p.TenantID] == nil {
			r.byTrade[p.TenantID] = make(map[string]int64)
		}
		r.byTrade[p.TenantID][p.OutTradeNo] = id
	}
	return clonePayment(stored), nil
}

// GetByID returns the payment for (tenantID, id), or ErrNotFound.
func (r *MemoryPaymentRepo) GetByID(_ context.Context, tenantID, id int64) (*Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.rows[id]
	if !ok || p.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return clonePayment(p), nil
}

// GetByOutTradeNo returns the payment for (tenantID, outTradeNo), or ErrNotFound.
func (r *MemoryPaymentRepo) GetByOutTradeNo(_ context.Context, tenantID int64, outTradeNo string) (*Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, ok := r.byTrade[tenantID]
	if !ok {
		return nil, ErrNotFound
	}
	id, ok := idx[outTradeNo]
	if !ok {
		return nil, ErrNotFound
	}
	p := r.rows[id]
	return clonePayment(p), nil
}

// GetByOutTradeNoAnyTenant scans every tenant's index for outTradeNo (the notify
// path cannot scope by tenant, CONTRACT §15.7). out_trade_no is registered per
// tenant so the first hit wins; ErrNotFound when no tenant has it.
func (r *MemoryPaymentRepo) GetByOutTradeNoAnyTenant(_ context.Context, outTradeNo string) (*Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, idx := range r.byTrade {
		if id, ok := idx[outTradeNo]; ok {
			return clonePayment(r.rows[id]), nil
		}
	}
	return nil, ErrNotFound
}

// MarkSuccess performs the idempotent pending→success transition:
// pending→success returns (p, true); already-success returns (p, false);
// anything else returns (nil, false, ErrStatusConflict); cross-tenant/absent
// returns (nil, false, ErrNotFound).
func (r *MemoryPaymentRepo) MarkSuccess(_ context.Context, tenantID, id int64, providerTxID string, paidAt time.Time) (*Payment, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.rows[id]
	if !ok || p.TenantID != tenantID {
		return nil, false, ErrNotFound
	}
	switch p.Status {
	case PaymentStatusSuccess:
		return clonePayment(p), false, nil
	case PaymentStatusPending:
		p.Status = PaymentStatusSuccess
		if providerTxID != "" {
			p.ProviderTxID = providerTxID
		}
		ac := paidAt
		p.PaidAt = &ac
		p.UpdatedAt = time.Now()
		return clonePayment(p), true, nil
	default:
		// failed/refunded cannot transition to success (irreversible state machine).
		return nil, false, ErrStatusConflict
	}
}

// UpdateStatus sets the payment status (used for failed notifies /
// admin-driven transitions). It does not touch PaidAt.
func (r *MemoryPaymentRepo) UpdateStatus(_ context.Context, tenantID, id int64, status PaymentStatus) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.rows[id]
	if !ok || p.TenantID != tenantID {
		return ErrNotFound
	}
	p.Status = status
	p.UpdatedAt = time.Now()
	return nil
}

// ===== Ledger =====

// MemoryLedgerRepo is the in-memory LedgerRepo (dev/test). It carries a
// per-tenant (provider, provider_tx_id) dedup index mirroring the DB UNIQUE
// constraint so a repeat callback returns the already-stored row.
type MemoryLedgerRepo struct {
	mu    sync.Mutex
	seq   int64
	rows  map[int64]*PaymentLedger
	dedup map[tenantProviderTx]int64 // (tenant,provider,tx_id) -> ledger id
}

// tenantProviderTx is the composite dedup key for the ledger.
type tenantProviderTx struct {
	tenantID int64
	provider string
	txID     string
}

// NewMemoryLedgerRepo constructs an empty MemoryLedgerRepo.
func NewMemoryLedgerRepo() *MemoryLedgerRepo {
	return &MemoryLedgerRepo{
		rows:  make(map[int64]*PaymentLedger),
		dedup: make(map[tenantProviderTx]int64),
	}
}

// Append is the idempotent ledger write. A non-empty provider_tx_id that
// already exists for (tenantID, provider, tx_id) returns the stored row (nil
// error); an empty provider_tx_id writes without dedup. Returns a copy.
func (r *MemoryLedgerRepo) Append(_ context.Context, l *PaymentLedger) (*PaymentLedger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.ProviderTxID != "" {
		key := tenantProviderTx{tenantID: l.TenantID, provider: l.Provider, txID: l.ProviderTxID}
		if id, ok := r.dedup[key]; ok {
			return cloneLedger(r.rows[id]), nil
		}
	}
	id := atomic.AddInt64(&r.seq, 1)
	now := time.Now()
	l.ID = id
	if l.CreatedAt.IsZero() {
		l.CreatedAt = now
	}
	stored := cloneLedger(l)
	r.rows[id] = stored
	if l.ProviderTxID != "" {
		r.dedup[tenantProviderTx{tenantID: l.TenantID, provider: l.Provider, txID: l.ProviderTxID}] = id
	}
	return cloneLedger(stored), nil
}

// ListByTenant returns up to limit ledger rows for tenantID, newest first
// (created_at desc) for the boss reconciliation view. limit<=0 defaults to 50.
func (r *MemoryLedgerRepo) ListByTenant(_ context.Context, tenantID int64, limit int) ([]*PaymentLedger, error) {
	if limit <= 0 {
		limit = 50
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*PaymentLedger, 0)
	for _, l := range r.rows {
		if l.TenantID == tenantID {
			out = append(out, cloneLedger(l))
		}
	}
	// Stable order: newest first by (CreatedAt, ID).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j], out[j-1]
			if a.CreatedAt.After(b.CreatedAt) || (a.CreatedAt.Equal(b.CreatedAt) && a.ID > b.ID) {
				out[j], out[j-1] = out[j-1], out[j]
			} else {
				break
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
