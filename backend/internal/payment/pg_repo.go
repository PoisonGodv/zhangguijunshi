package payment

// pg_repo.go implements the three PG-backed repos (CONTRACT §15.3.1 / §15.5.7)
// over the narrow PGPool surface (QueryRow / Exec), the same idiom as
// attribution.PGPool / bandit.PGRepo. Multi-row reads type-assert the concrete
// pool to queryer (pgxmock.PgxPoolIface / *pgxpool.Pool both satisfy it) so the
// frozen PGPool interface stays minimal. A nil pool is a safe no-op so a
// misconfigured main thread degrades to ErrNotFound / empty rather than
// panicking.
//
// Tenant isolation lives in every statement's WHERE tenant_id = $1. Nullable
// columns (provider_tx_id / paid_at / approved_at / sub_mch_id) are scanned via
// database/sql Null types; JSONB columns (materials / raw_payload) round-trip
// as []byte.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface the PG repos need (CONTRACT §15.3.1).
// Structurally identical to attribution.PGPool so the main thread can share one
// *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool
// and pgxmock expose on top of PGPool. The PGPool interface signature is
// frozen (no Query), so list methods type-assert the concrete pool to queryer;
// a pool lacking Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, q string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// SQL for sub_merchants (migration 000012).
const (
	sqlSubMerchantCreate = `INSERT INTO sub_merchants (tenant_id, store_id, provider, applyment_id, status, rate_bps, materials, applied_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`
	sqlSubMerchantGetByID = `SELECT id, tenant_id, store_id, provider, sub_mch_id, applyment_id, status, rate_bps, materials, applied_at, approved_at, updated_at
FROM sub_merchants
WHERE tenant_id = $1 AND id = $2`
	sqlSubMerchantGetActive = `SELECT id, tenant_id, store_id, provider, sub_mch_id, applyment_id, status, rate_bps, materials, applied_at, approved_at, updated_at
FROM sub_merchants
WHERE tenant_id = $1 AND store_id = $2 AND provider = $3 AND status = 'active'
ORDER BY applied_at DESC
LIMIT 1`
	sqlSubMerchantList = `SELECT id, tenant_id, store_id, provider, sub_mch_id, applyment_id, status, rate_bps, materials, applied_at, approved_at, updated_at
FROM sub_merchants
WHERE tenant_id = $1
ORDER BY id`
	sqlSubMerchantUpdateStatus = `UPDATE sub_merchants SET status = $1, sub_mch_id = COALESCE(NULLIF($2, ''), sub_mch_id), approved_at = $3, updated_at = now() WHERE tenant_id = $4 AND id = $5`
)

// SQL for payments (migration 000012). MarkSuccess is a two-step
// read-then-conditional-update so the changed flag and ErrStatusConflict are
// accurate.
const (
	sqlPaymentCreate        = `INSERT INTO payments (tenant_id, store_id, sub_merchant_id, out_trade_no, provider, provider_tx_id, subject, ref_id, amount_cents, type, status) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`
	sqlPaymentGetByID       = `SELECT id, tenant_id, store_id, sub_merchant_id, out_trade_no, provider, provider_tx_id, subject, ref_id, amount_cents, type, status, paid_at, created_at, updated_at FROM payments WHERE tenant_id = $1 AND id = $2`
	sqlPaymentGetByTrade    = `SELECT id, tenant_id, store_id, sub_merchant_id, out_trade_no, provider, provider_tx_id, subject, ref_id, amount_cents, type, status, paid_at, created_at, updated_at FROM payments WHERE tenant_id = $1 AND out_trade_no = $2`
	sqlPaymentGetByTradeAny = `SELECT id, tenant_id, store_id, sub_merchant_id, out_trade_no, provider, provider_tx_id, subject, ref_id, amount_cents, type, status, paid_at, created_at, updated_at FROM payments WHERE out_trade_no = $1 LIMIT 1`
	sqlPaymentGetForMark    = `SELECT id, tenant_id, store_id, sub_merchant_id, out_trade_no, provider, provider_tx_id, subject, ref_id, amount_cents, type, status, paid_at, created_at, updated_at FROM payments WHERE tenant_id = $1 AND id = $2`
	sqlPaymentMarkSuccess   = `UPDATE payments SET status = 'success', provider_tx_id = $1, paid_at = $2, updated_at = now() WHERE tenant_id = $3 AND id = $4 AND status = 'pending' RETURNING id, tenant_id, store_id, sub_merchant_id, out_trade_no, provider, provider_tx_id, subject, ref_id, amount_cents, type, status, paid_at, created_at, updated_at`
	sqlPaymentUpdateStat    = `UPDATE payments SET status = $1, updated_at = now() WHERE tenant_id = $2 AND id = $3`
)

// SQL for payment_ledger (migration 000012). Append is an idempotent upsert:
// ON CONFLICT (tenant_id, provider, provider_tx_id) DO NOTHING. An empty
// provider_tx_id is stored as NULL so it never conflicts (PG NULL ≠ NULL),
// matching the contract "空→不去重".
const (
	sqlLedgerAppend = `INSERT INTO payment_ledger (tenant_id, store_id, sub_merchant_id, order_id, provider, provider_tx_id, amount_cents, fee_cents, type, status, raw_payload, paid_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (tenant_id, provider, provider_tx_id) DO NOTHING
RETURNING id, tenant_id, store_id, sub_merchant_id, order_id, provider, provider_tx_id, amount_cents, fee_cents, type, status, raw_payload, paid_at, created_at`
	sqlLedgerGetByTx = `SELECT id, tenant_id, store_id, sub_merchant_id, order_id, provider, provider_tx_id, amount_cents, fee_cents, type, status, raw_payload, paid_at, created_at
FROM payment_ledger
WHERE tenant_id = $1 AND provider = $2 AND provider_tx_id = $3
LIMIT 1`
	sqlLedgerList = `SELECT id, tenant_id, store_id, sub_merchant_id, order_id, provider, provider_tx_id, amount_cents, fee_cents, type, status, raw_payload, paid_at, created_at
FROM payment_ledger
WHERE tenant_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2`
)

// ===== scan helpers =====

// scanSubMerchant decodes one sub_merchants row (JSONB materials → map).
func scanSubMerchant(s rowScanner) (*SubMerchant, error) {
	var sm SubMerchant
	var subMchID sql.NullString
	var status string
	var materialsRaw []byte
	var approvedAt sql.NullTime
	if err := s.Scan(
		&sm.ID, &sm.TenantID, &sm.StoreID, &sm.Provider, &subMchID, &sm.ApplymentID,
		&status, &sm.RateBPS, &materialsRaw, &sm.AppliedAt, &approvedAt, &sm.UpdatedAt,
	); err != nil {
		return nil, err
	}
	sm.SubMchID = subMchID.String
	sm.Status = SubMerchantStatus(status)
	if approvedAt.Valid {
		t := approvedAt.Time
		sm.ApprovedAt = &t
	}
	if len(materialsRaw) > 0 {
		_ = json.Unmarshal(materialsRaw, &sm.Materials)
	}
	return &sm, nil
}

// scanPayment decodes one payments row.
func scanPayment(s rowScanner) (*Payment, error) {
	var p Payment
	var providerTxID sql.NullString
	var ptype, pstatus string
	var paidAt sql.NullTime
	if err := s.Scan(
		&p.ID, &p.TenantID, &p.StoreID, &p.SubMerchantID, &p.OutTradeNo, &p.Provider,
		&providerTxID, &p.Subject, &p.RefID, &p.AmountCents, &ptype, &pstatus,
		&paidAt, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	p.ProviderTxID = providerTxID.String
	p.Type = PaymentType(ptype)
	p.Status = PaymentStatus(pstatus)
	if paidAt.Valid {
		t := paidAt.Time
		p.PaidAt = &t
	}
	return &p, nil
}

// scanLedger decodes one payment_ledger row (JSONB raw_payload → []byte).
func scanLedger(s rowScanner) (*PaymentLedger, error) {
	var l PaymentLedger
	var providerTxID sql.NullString
	var ltype string
	var raw []byte
	var paidAt sql.NullTime
	if err := s.Scan(
		&l.ID, &l.TenantID, &l.StoreID, &l.SubMerchantID, &l.OrderID, &l.Provider,
		&providerTxID, &l.AmountCents, &l.FeeCents, &ltype, &l.Status, &raw,
		&paidAt, &l.CreatedAt,
	); err != nil {
		return nil, err
	}
	l.ProviderTxID = providerTxID.String
	l.Type = PaymentType(ltype)
	l.RawPayload = raw
	if paidAt.Valid {
		t := paidAt.Time
		l.PaidAt = &t
	}
	return &l, nil
}

// nullTime converts a time.Time to sql.NullTime (zero → NULL).
func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// ===== SubMerchant =====

// PGSubMerchantRepo is the pgx-backed SubMerchantRepo. A nil pool makes reads
// return ErrNotFound and writes return ErrNotFound.
type PGSubMerchantRepo struct{ pool PGPool }

// NewPGSubMerchantRepo constructs a PGSubMerchantRepo over pool.
func NewPGSubMerchantRepo(pool PGPool) *PGSubMerchantRepo {
	return &PGSubMerchantRepo{pool: pool}
}

// Create inserts sm and returns a copy with the allocated id.
func (r *PGSubMerchantRepo) Create(ctx context.Context, sm *SubMerchant) (*SubMerchant, error) {
	if r == nil || r.pool == nil || sm == nil {
		return nil, ErrNotFound
	}
	if err := r.pool.QueryRow(ctx, sqlSubMerchantCreate,
		sm.TenantID, sm.StoreID, sm.Provider, sm.ApplymentID, sm.Status, sm.RateBPS, sm.Materials, sm.AppliedAt,
	).Scan(&sm.ID); err != nil {
		return nil, fmt.Errorf("payment: pg sub-merchant create: %w", err)
	}
	return cloneSubMerchant(sm), nil
}

// GetByID loads the sub-merchant for (tenantID, id); ErrNotFound when absent or
// cross-tenant.
func (r *PGSubMerchantRepo) GetByID(ctx context.Context, tenantID, id int64) (*SubMerchant, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	sm, err := scanSubMerchant(r.pool.QueryRow(ctx, sqlSubMerchantGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("payment: pg sub-merchant get: %w", err)
	}
	return sm, nil
}

// GetActiveByStore returns the active sub-merchant for (tenantID, storeID,
// provider), or ErrNotFound.
func (r *PGSubMerchantRepo) GetActiveByStore(ctx context.Context, tenantID, storeID int64, provider string) (*SubMerchant, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	sm, err := scanSubMerchant(r.pool.QueryRow(ctx, sqlSubMerchantGetActive, tenantID, storeID, provider))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("payment: pg sub-merchant active: %w", err)
	}
	return sm, nil
}

// UpdateStatus advances onboarding state. approvedAt zero leaves ApprovedAt
// NULL; non-zero sets it. An empty subMchID leaves the existing value (COALESCE
// over NULLIF turns ” into NULL).
func (r *PGSubMerchantRepo) UpdateStatus(ctx context.Context, tenantID, id int64, status SubMerchantStatus, subMchID string, approvedAt time.Time) error {
	if r == nil || r.pool == nil {
		return ErrNotFound
	}
	if _, err := r.pool.Exec(ctx, sqlSubMerchantUpdateStatus,
		status, subMchID, nullTime(approvedAt), tenantID, id,
	); err != nil {
		return fmt.Errorf("payment: pg sub-merchant update status: %w", err)
	}
	return nil
}

// ListByTenant returns all sub-merchants for tenantID ordered by id.
func (r *PGSubMerchantRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*SubMerchant, error) {
	out := make([]*SubMerchant, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlSubMerchantList, tenantID)
	if err != nil {
		return nil, fmt.Errorf("payment: pg sub-merchant list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		sm, err := scanSubMerchant(rows)
		if err != nil {
			return nil, fmt.Errorf("payment: pg sub-merchant list scan: %w", err)
		}
		out = append(out, sm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payment: pg sub-merchant list rows: %w", err)
	}
	return out, nil
}

// ===== Payment =====

// PGPaymentRepo is the pgx-backed PaymentRepo.
type PGPaymentRepo struct{ pool PGPool }

// NewPGPaymentRepo constructs a PGPaymentRepo over pool.
func NewPGPaymentRepo(pool PGPool) *PGPaymentRepo { return &PGPaymentRepo{pool: pool} }

// Create inserts p and returns a copy with the allocated id.
func (r *PGPaymentRepo) Create(ctx context.Context, p *Payment) (*Payment, error) {
	if r == nil || r.pool == nil || p == nil {
		return nil, ErrNotFound
	}
	if err := r.pool.QueryRow(ctx, sqlPaymentCreate,
		p.TenantID, p.StoreID, p.SubMerchantID, p.OutTradeNo, p.Provider, p.ProviderTxID,
		p.Subject, p.RefID, p.AmountCents, p.Type, p.Status,
	).Scan(&p.ID); err != nil {
		return nil, fmt.Errorf("payment: pg payment create: %w", err)
	}
	return clonePayment(p), nil
}

// GetByID loads the payment for (tenantID, id); ErrNotFound when absent or
// cross-tenant.
func (r *PGPaymentRepo) GetByID(ctx context.Context, tenantID, id int64) (*Payment, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	p, err := scanPayment(r.pool.QueryRow(ctx, sqlPaymentGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("payment: pg payment get: %w", err)
	}
	return p, nil
}

// GetByOutTradeNo loads the payment for (tenantID, outTradeNo).
func (r *PGPaymentRepo) GetByOutTradeNo(ctx context.Context, tenantID int64, outTradeNo string) (*Payment, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	p, err := scanPayment(r.pool.QueryRow(ctx, sqlPaymentGetByTrade, tenantID, outTradeNo))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("payment: pg payment get by trade: %w", err)
	}
	return p, nil
}

// GetByOutTradeNoAnyTenant loads a payment by out_trade_no across all tenants
// (notify-callback tenant resolution, CONTRACT §15.7). out_trade_no is globally
// unique per tenant; the notify path cannot scope by tenant, so this query omits
// the tenant filter. ErrNotFound when no payment matches.
func (r *PGPaymentRepo) GetByOutTradeNoAnyTenant(ctx context.Context, outTradeNo string) (*Payment, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	p, err := scanPayment(r.pool.QueryRow(ctx, sqlPaymentGetByTradeAny, outTradeNo))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("payment: pg payment get by trade any-tenant: %w", err)
	}
	return p, nil
}

// MarkSuccess performs the idempotent pending→success transition: read current
// status, then conditional UPDATE. pending→success returns (p, true);
// already-success returns (p, false); other states return ErrStatusConflict;
// absent/cross-tenant returns ErrNotFound.
func (r *PGPaymentRepo) MarkSuccess(ctx context.Context, tenantID, id int64, providerTxID string, paidAt time.Time) (*Payment, bool, error) {
	if r == nil || r.pool == nil {
		return nil, false, ErrNotFound
	}
	current, err := scanPayment(r.pool.QueryRow(ctx, sqlPaymentGetForMark, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, fmt.Errorf("payment: pg mark-success load: %w", err)
	}
	switch current.Status {
	case PaymentStatusSuccess:
		return current, false, nil
	case PaymentStatusPending:
		updated, err := scanPayment(r.pool.QueryRow(ctx, sqlPaymentMarkSuccess, providerTxID, paidAt, tenantID, id))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Raced: another writer flipped it out of pending between the
				// read and the conditional update. Treat as conflict.
				return nil, false, ErrStatusConflict
			}
			return nil, false, fmt.Errorf("payment: pg mark-success update: %w", err)
		}
		return updated, true, nil
	default:
		return nil, false, ErrStatusConflict
	}
}

// UpdateStatus sets the payment status (failed-notify / admin path).
func (r *PGPaymentRepo) UpdateStatus(ctx context.Context, tenantID, id int64, status PaymentStatus) error {
	if r == nil || r.pool == nil {
		return ErrNotFound
	}
	if _, err := r.pool.Exec(ctx, sqlPaymentUpdateStat, status, tenantID, id); err != nil {
		return fmt.Errorf("payment: pg payment update status: %w", err)
	}
	return nil
}

// ===== Ledger =====

// PGLedgerRepo is the pgx-backed LedgerRepo.
type PGLedgerRepo struct{ pool PGPool }

// NewPGLedgerRepo constructs a PGLedgerRepo over pool.
func NewPGLedgerRepo(pool PGPool) *PGLedgerRepo { return &PGLedgerRepo{pool: pool} }

// Append is the idempotent ledger write. INSERT ... ON CONFLICT DO NOTHING: a
// row returned means inserted; pgx.ErrNoRows means a concurrent/duplicate row
// already exists (UNIQUE tx_id), which is then read back and returned. An
// empty provider_tx_id is stored as NULL so it never conflicts.
func (r *PGLedgerRepo) Append(ctx context.Context, l *PaymentLedger) (*PaymentLedger, error) {
	if r == nil || r.pool == nil {
		return cloneLedger(l), nil
	}
	var txID any = l.ProviderTxID
	if l.ProviderTxID == "" {
		txID = nil
	}
	inserted, err := scanLedger(r.pool.QueryRow(ctx, sqlLedgerAppend,
		l.TenantID, l.StoreID, l.SubMerchantID, l.OrderID, l.Provider, txID,
		l.AmountCents, l.FeeCents, l.Type, l.Status, l.RawPayload, l.PaidAt,
	))
	if err == nil {
		return inserted, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Conflict: dedup — read the already-stored row by (tenant, provider,
		// provider_tx_id). Empty tx_id never conflicts, so this branch is only
		// reached for a non-empty duplicate.
		existing, err := scanLedger(r.pool.QueryRow(ctx, sqlLedgerGetByTx, l.TenantID, l.Provider, l.ProviderTxID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return cloneLedger(l), nil
			}
			return nil, fmt.Errorf("payment: pg ledger dedup read: %w", err)
		}
		return existing, nil
	}
	return nil, fmt.Errorf("payment: pg ledger append: %w", err)
}

// ListByTenant returns up to limit ledger rows for tenantID, newest first.
// limit<=0 defaults to 50.
func (r *PGLedgerRepo) ListByTenant(ctx context.Context, tenantID int64, limit int) ([]*PaymentLedger, error) {
	out := make([]*PaymentLedger, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.Query(ctx, sqlLedgerList, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("payment: pg ledger list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanLedger(rows)
		if err != nil {
			return nil, fmt.Errorf("payment: pg ledger list scan: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("payment: pg ledger list rows: %w", err)
	}
	return out, nil
}
