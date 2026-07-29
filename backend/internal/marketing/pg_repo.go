package marketing

// pg_repo.go implements the pgx-backed CouponRepo (CONTRACT §17.2 / §10.3) over
// the narrow PGPool surface (QueryRow / Exec), mirroring the payment pg_repo
// idiom one-for-one. Multi-row reads type-assert the concrete pool to queryer
// (pgxmock.PgxPoolIface / *pgxpool.Pool both satisfy it) so the frozen PGPool
// interface stays minimal. A nil pool is a safe no-op so a misconfigured main
// thread degrades to ErrNotFound / empty rather than panicking.
//
// Tenant isolation lives in every statement's WHERE tenant_id = $N (ARCH-01);
// cross-tenant reads are indistinguishable from absence (ErrNotFound). Nullable
// columns (store_id / total_quota / created_by / strategy_id / used_at /
// expire_at / ref_order_id) are scanned via database/sql Null types, mapping
// SQL NULL ↔ Go zero (store_id/total_quota/created_by/strategy_id/ref_order_id
// use 0 as the "unset" sentinel on the Go side). The coupons.params JSONB
// column round-trips as []byte through CouponParams.

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

// PGPool is the narrow database surface the PG repo needs (CONTRACT §17.2.1).
// Structurally identical to payment.PGPool so the main thread can share one
// *pgxpool.Pool across packages.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool
// and pgxmock expose on top of PGPool. The PGPool interface signature is frozen
// (no Query), so list methods type-assert the concrete pool to queryer; a pool
// lacking Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, q string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// Compile-time guarantee that PGCouponRepo satisfies the frozen CouponRepo.
var _ CouponRepo = (*PGCouponRepo)(nil)

// SQL for coupons (migration 000005). coupons is soft-delete capable
// (deleted_at + partial unique index), so every read/update filters
// deleted_at IS NULL — production-correct and harmless in the dev path where
// the repo never soft-deletes. total_quota is nullable (NULL = unlimited),
// mirroring the Go TotalQuota==0 sentinel.
const (
	sqlCouponSelectCols = "id, tenant_id, store_id, name, type, params, valid_from, valid_to, total_quota, issued, status, created_by, created_at, updated_at"

	sqlCouponCreate = `INSERT INTO coupons (tenant_id, store_id, name, type, params, valid_from, valid_to, total_quota, status, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, created_at, updated_at`

	sqlCouponGetByID = `SELECT ` + sqlCouponSelectCols + ` FROM coupons WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`

	sqlCouponGetByName = `SELECT ` + sqlCouponSelectCols + ` FROM coupons WHERE tenant_id = $1 AND name = $2 AND deleted_at IS NULL`

	// sqlCouponIncrementIssued is the atomic quota gate: issued increments only
	// while the row is still under quota (or unlimited). RETURNING issued lets
	// the caller confirm the new count; pgx.ErrNoRows means quota exhausted or
	// the coupon vanished between the read and the conditional update.
	sqlCouponIncrementIssued = `UPDATE coupons SET issued = issued + 1, updated_at = now() WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL AND (total_quota IS NULL OR issued < total_quota) RETURNING issued`

	// sqlCouponUpdate rewrites the template fields (status/issued included per
	// CONTRACT §10.3) by ID+tenant. RETURNING the full row gives the caller the
	// trigger-refreshed updated_at. 0 rows → ErrNotFound (absent/cross-tenant).
	sqlCouponUpdate = `UPDATE coupons SET store_id = $1, name = $2, type = $3, params = $4, valid_from = $5, valid_to = $6, total_quota = $7, status = $8, issued = $9, created_by = $10, updated_at = now() WHERE tenant_id = $11 AND id = $12 AND deleted_at IS NULL RETURNING ` + sqlCouponSelectCols
)

// SQL for member_coupons (migration 000005). Audit table: no updated_at, no
// soft delete. strategy_id / used_at / expire_at / ref_order_id are nullable.
const (
	sqlMCSelectCols = "id, tenant_id, member_id, coupon_id, status, source, strategy_id, issued_at, used_at, expire_at, ref_order_id"

	sqlMCInsert = `INSERT INTO member_coupons (tenant_id, member_id, coupon_id, status, source, strategy_id, issued_at, expire_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING ` + sqlMCSelectCols

	sqlMCGetByID = `SELECT ` + sqlMCSelectCols + ` FROM member_coupons WHERE tenant_id = $1 AND id = $2`

	// sqlMCRedeem is the atomic unused→used transition (CONTRACT §10.3). The
	// status = 'unused' guard makes it race-safe; pgx.ErrNoRows means another
	// writer redeemed it first → ErrCouponUsed.
	sqlMCRedeem = `UPDATE member_coupons SET status = 'used', used_at = $1, ref_order_id = $2 WHERE tenant_id = $3 AND id = $4 AND status = 'unused' RETURNING ` + sqlMCSelectCols

	// sqlMCMarkExpired persists the expired side-effect memory's RedeemAtomic
	// applies when an unused member-coupon is past its expire cutoff.
	sqlMCMarkExpired = `UPDATE member_coupons SET status = 'expired' WHERE tenant_id = $1 AND id = $2 AND status = 'unused'`
)

// ===== scan helpers =====

// scanCoupon decodes one coupons row (JSONB params → CouponParams; nullable
// store_id / total_quota / created_by → 0).
func scanCoupon(s rowScanner) (*Coupon, error) {
	var c Coupon
	var ctype, status string
	var paramsRaw []byte
	var storeID, totalQuota, createdBy sql.NullInt64
	if err := s.Scan(
		&c.ID, &c.TenantID, &storeID, &c.Name, &ctype, &paramsRaw,
		&c.ValidFrom, &c.ValidTo, &totalQuota, &c.Issued, &status, &createdBy,
		&c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		return nil, err
	}
	c.StoreID = storeID.Int64
	c.TotalQuota = totalQuota.Int64
	c.CreatedBy = createdBy.Int64
	c.Type = ctype
	c.Status = status
	if len(paramsRaw) > 0 {
		_ = json.Unmarshal(paramsRaw, &c.Params)
	}
	return &c, nil
}

// scanMemberCoupon decodes one member_coupons row (nullable strategy_id /
// used_at / expire_at / ref_order_id → 0 / nil).
func scanMemberCoupon(s rowScanner) (*MemberCoupon, error) {
	var mc MemberCoupon
	var status, source string
	var strategyID, refOrderID sql.NullInt64
	var usedAt, expireAt sql.NullTime
	if err := s.Scan(
		&mc.ID, &mc.TenantID, &mc.MemberID, &mc.CouponID, &status, &source,
		&strategyID, &mc.IssuedAt, &usedAt, &expireAt, &refOrderID,
	); err != nil {
		return nil, err
	}
	mc.Status = status
	mc.Source = source
	mc.StrategyID = strategyID.Int64
	mc.RefOrderID = refOrderID.Int64
	if usedAt.Valid {
		t := usedAt.Time
		mc.UsedAt = &t
	}
	if expireAt.Valid {
		t := expireAt.Time
		mc.ExpireAt = &t
	}
	return &mc, nil
}

// nullIntID maps a Go int64 id to the SQL write side: 0 (the "unset" sentinel
// for store_id / total_quota / created_by / strategy_id / ref_order_id) becomes
// SQL NULL so nullable / FK columns stay valid; non-zero passes through. This
// mirrors payment's `var txID any` nil-on-empty pattern.
func nullIntID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// buildCouponListQuery renders the ListCoupons SQL + args for a filter. The
// tenant placeholder is always $1; optional filters append in a fixed order
// (store_id, status, type) so the string is deterministic — pgxmock's
// QueryMatcherEqual needs the exact text, and tests call this same helper to
// set expectations.
func buildCouponListQuery(tenantID int64, f CouponFilter) (string, []any) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	q := "SELECT " + sqlCouponSelectCols + " FROM coupons WHERE tenant_id = $1 AND deleted_at IS NULL"
	args := []any{tenantID}
	n := 2
	if f.StoreID > 0 {
		q += fmt.Sprintf(" AND store_id = $%d", n)
		args = append(args, f.StoreID)
		n++
	}
	if f.Status != "" {
		q += fmt.Sprintf(" AND status = $%d", n)
		args = append(args, f.Status)
		n++
	}
	if f.Type != "" {
		q += fmt.Sprintf(" AND type = $%d", n)
		args = append(args, f.Type)
		n++
	}
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT $%d", n)
	args = append(args, limit)
	n++
	if f.Offset > 0 {
		q += fmt.Sprintf(" OFFSET $%d", n)
		args = append(args, f.Offset)
	}
	return q, args
}

// buildMCListQuery renders the ListMemberCoupons / ListByStrategy SQL + args.
// filterCol is a hardcoded column name ("member_id" / "strategy_id"), never
// caller input, so concatenating it is safe. Optional status appends as $3.
func buildMCListQuery(filterCol string, tenantID, filterVal int64, status string) (string, []any) {
	q := "SELECT " + sqlMCSelectCols + " FROM member_coupons WHERE tenant_id = $1 AND " + filterCol + " = $2"
	args := []any{tenantID, filterVal}
	if status != "" {
		q += " AND status = $3"
		args = append(args, status)
	}
	q += " ORDER BY id DESC"
	return q, args
}

// ===== PGCouponRepo =====

// PGCouponRepo is the pgx-backed CouponRepo. A nil pool makes reads return
// ErrNotFound, writes return ErrNotFound, and lists return an empty slice.
type PGCouponRepo struct{ pool PGPool }

// NewPGCouponRepo constructs a PGCouponRepo over pool.
func NewPGCouponRepo(pool PGPool) *PGCouponRepo { return &PGCouponRepo{pool: pool} }

// CreateCoupon inserts c and returns a copy with the DB-allocated id and
// timestamps. created_at / updated_at are left to the DB DEFAULT now() (DB is
// the source of truth); an empty Status defaults to draft so the CHECK
// constraint can never reject the row.
func (r *PGCouponRepo) CreateCoupon(ctx context.Context, c *Coupon) (*Coupon, error) {
	if r == nil || r.pool == nil || c == nil {
		return nil, ErrNotFound
	}
	status := c.Status
	if status == "" {
		status = StatusDraft
	}
	params, err := json.Marshal(c.Params)
	if err != nil {
		return nil, fmt.Errorf("marketing: pg coupon create marshal params: %w", err)
	}
	if err := r.pool.QueryRow(ctx, sqlCouponCreate,
		c.TenantID, nullIntID(c.StoreID), c.Name, c.Type, params,
		c.ValidFrom, c.ValidTo, nullIntID(c.TotalQuota), status, nullIntID(c.CreatedBy),
	).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, fmt.Errorf("marketing: pg coupon create: %w", err)
	}
	c.Status = status
	return cloneCoupon(c), nil
}

// GetCoupon loads the coupon for (tenantID, id); ErrNotFound when absent,
// soft-deleted, or owned by another tenant.
func (r *PGCouponRepo) GetCoupon(ctx context.Context, tenantID, id int64) (*Coupon, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	c, err := scanCoupon(r.pool.QueryRow(ctx, sqlCouponGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketing: pg coupon get: %w", err)
	}
	return c, nil
}

// GetByName loads the coupon for (tenantID, name); ErrNotFound when absent.
// Respects the partial unique index (deleted_at IS NULL).
func (r *PGCouponRepo) GetByName(ctx context.Context, tenantID int64, name string) (*Coupon, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	c, err := scanCoupon(r.pool.QueryRow(ctx, sqlCouponGetByName, tenantID, name))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketing: pg coupon get by name: %w", err)
	}
	return c, nil
}

// UpdateCoupon rewrites the template fields (status/issued per CONTRACT §10.3)
// by ID+tenant and returns the refreshed row. ErrNotFound when absent,
// soft-deleted, or cross-tenant (RETURNING yields no rows).
func (r *PGCouponRepo) UpdateCoupon(ctx context.Context, c *Coupon) (*Coupon, error) {
	if r == nil || r.pool == nil || c == nil {
		return nil, ErrNotFound
	}
	params, err := json.Marshal(c.Params)
	if err != nil {
		return nil, fmt.Errorf("marketing: pg coupon update marshal params: %w", err)
	}
	updated, err := scanCoupon(r.pool.QueryRow(ctx, sqlCouponUpdate,
		nullIntID(c.StoreID), c.Name, c.Type, params, c.ValidFrom, c.ValidTo,
		nullIntID(c.TotalQuota), c.Status, c.Issued, nullIntID(c.CreatedBy),
		c.TenantID, c.ID,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketing: pg coupon update: %w", err)
	}
	return updated, nil
}

// ListCoupons returns coupons matching f, newest first. Zero-value filter
// fields are not applied; Limit<=0 defaults to 50.
func (r *PGCouponRepo) ListCoupons(ctx context.Context, tenantID int64, f CouponFilter) ([]*Coupon, error) {
	out := make([]*Coupon, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	sqlStr, args := buildCouponListQuery(tenantID, f)
	rows, err := q.Query(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("marketing: pg coupon list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanCoupon(rows)
		if err != nil {
			return nil, fmt.Errorf("marketing: pg coupon list scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketing: pg coupon list rows: %w", err)
	}
	return out, nil
}

// IssueToMember atomically validates the coupon and creates one member-coupon.
// Flow (mirrors MemoryCouponRepo + payment.MarkSuccess two-step):
//  1. Load coupon (tenant-scoped); ErrNotFound when absent/cross-tenant.
//  2. Go validation: active + within valid window → else ErrCouponInactive.
//  3. Quota pre-check: issued>=quota → ErrQuotaExhausted (clear error, no write).
//  4. Conditional UPDATE issued=issued+1 WHERE under quota (race-safe);
//     pgx.ErrNoRows → quota raced → ErrQuotaExhausted.
//  5. Resolve expire (caller's expireAt else coupon.ValidTo) and INSERT the
//     member-coupon, RETURNING the full row.
func (r *PGCouponRepo) IssueToMember(ctx context.Context, tenantID, memberID, couponID int64, source string, strategyID int64, expireAt *time.Time, now time.Time) (*MemberCoupon, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	c, err := scanCoupon(r.pool.QueryRow(ctx, sqlCouponGetByID, tenantID, couponID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketing: pg issue load coupon: %w", err)
	}
	if c.Status != StatusActive {
		return nil, ErrCouponInactive
	}
	if now.Before(c.ValidFrom) || now.After(c.ValidTo) {
		return nil, ErrCouponInactive
	}
	if c.TotalQuota > 0 && c.Issued >= c.TotalQuota {
		return nil, ErrQuotaExhausted
	}
	var newIssued int64
	if err := r.pool.QueryRow(ctx, sqlCouponIncrementIssued, tenantID, couponID).Scan(&newIssued); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrQuotaExhausted
		}
		return nil, fmt.Errorf("marketing: pg issue increment: %w", err)
	}
	exp := expireAt
	if exp == nil {
		v := c.ValidTo
		exp = &v
	}
	src := source
	if src == "" {
		src = SourceManual
	}
	mc, err := scanMemberCoupon(r.pool.QueryRow(ctx, sqlMCInsert,
		tenantID, memberID, couponID, MCStatusUnused, src, nullIntID(strategyID), now, *exp,
	))
	if err != nil {
		return nil, fmt.Errorf("marketing: pg issue insert member-coupon: %w", err)
	}
	return mc, nil
}

// GetMemberCoupon loads one member-coupon by id within tenantID; ErrNotFound
// when absent or owned by another tenant.
func (r *PGCouponRepo) GetMemberCoupon(ctx context.Context, tenantID, id int64) (*MemberCoupon, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	mc, err := scanMemberCoupon(r.pool.QueryRow(ctx, sqlMCGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketing: pg member-coupon get: %w", err)
	}
	return mc, nil
}

// ListMemberCoupons returns a member's coupons (optionally filtered by status),
// newest first. status=="" returns all statuses.
func (r *PGCouponRepo) ListMemberCoupons(ctx context.Context, tenantID, memberID int64, status string) ([]*MemberCoupon, error) {
	return r.listMC(ctx, "member_id", tenantID, memberID, status)
}

// ListByStrategy returns all member-coupons attributed to one strategy
// (member_coupons.strategy_id), newest first. status=="" returns all statuses.
// Cross-tenant records are never returned.
func (r *PGCouponRepo) ListByStrategy(ctx context.Context, tenantID, strategyID int64, status string) ([]*MemberCoupon, error) {
	return r.listMC(ctx, "strategy_id", tenantID, strategyID, status)
}

// listMC is the shared multi-row reader for the two member-coupon list methods.
func (r *PGCouponRepo) listMC(ctx context.Context, filterCol string, tenantID, filterVal int64, status string) ([]*MemberCoupon, error) {
	out := make([]*MemberCoupon, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	sqlStr, args := buildMCListQuery(filterCol, tenantID, filterVal, status)
	rows, err := q.Query(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("marketing: pg member-coupon list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		mc, err := scanMemberCoupon(rows)
		if err != nil {
			return nil, fmt.Errorf("marketing: pg member-coupon list scan: %w", err)
		}
		out = append(out, mc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("marketing: pg member-coupon list rows: %w", err)
	}
	return out, nil
}

// RedeemAtomic transitions a member-coupon unused→used and stamps UsedAt /
// RefOrderID. Flow (mirrors MemoryCouponRepo):
//  1. Load member-coupon (tenant-scoped); ErrNotFound when absent/cross-tenant.
//  2. Non-unused → ErrCouponUsed.
//  3. Resolve expire: mc.ExpireAt, else the coupon's ValidTo (same tenant).
//  4. Past expire → persist status=expired (memory side-effect) + ErrCouponExpired.
//  5. Conditional UPDATE unused→used; pgx.ErrNoRows → raced → ErrCouponUsed.
func (r *PGCouponRepo) RedeemAtomic(ctx context.Context, tenantID, id, refOrderID int64, now time.Time) (*MemberCoupon, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	m, err := scanMemberCoupon(r.pool.QueryRow(ctx, sqlMCGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("marketing: pg redeem load: %w", err)
	}
	if m.Status != MCStatusUnused {
		return nil, ErrCouponUsed
	}
	exp := m.ExpireAt
	if exp == nil {
		if c, err := scanCoupon(r.pool.QueryRow(ctx, sqlCouponGetByID, tenantID, m.CouponID)); err == nil {
			v := c.ValidTo
			exp = &v
		}
		// Coupon absent/cross-tenant/deleted → exp stays nil → no expiry check
		// (mirror memory, which only falls back when the coupon is in-tenant).
	}
	if exp != nil && now.After(*exp) {
		if _, err := r.pool.Exec(ctx, sqlMCMarkExpired, tenantID, id); err != nil {
			return nil, fmt.Errorf("marketing: pg redeem mark expired: %w", err)
		}
		return nil, ErrCouponExpired
	}
	mc, err := scanMemberCoupon(r.pool.QueryRow(ctx, sqlMCRedeem, now, nullIntID(refOrderID), tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCouponUsed
		}
		return nil, fmt.Errorf("marketing: pg redeem update: %w", err)
	}
	return mc, nil
}
