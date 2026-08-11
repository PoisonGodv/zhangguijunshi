package inventory

// pg_repo.go implements the pgx-backed BatchRepo plus the optional StockConsumer
// capability (CONTRACT §9.3 / §17). It mirrors the payment pg_repo idiom
// verbatim: a narrow PGPool surface (QueryRow / Exec), an optional queryer
// type-assert for multi-row reads, scan helpers over rowScanner, nil-pool safe
// degradation (read -> ErrNotFound / empty, write -> ErrNotFound), and
// pgx.ErrNoRows mapped to the domain ErrNotFound. Every read/write statement
// carries tenant_id in its WHERE clause (ARCH-01 row isolation).
//
// ConsumeFIFOAtomic is the one method that needs a real transaction: FIFO
// deduction touches several batches and appends one sale_out movement per lot,
// which must be all-or-nothing with no oversell. inventory does NOT import pkg/db
// (that would couple a domain package to infra), so the transaction is driven
// through a local txBeginner type-assert over the pool — both *pgxpool.Pool and
// pgxmock satisfy it — implementing the "SELECT ... FOR UPDATE txn" the domain
// doc prescribes. mkMovement / clone helpers are shared with memory_repo.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PGPool is the narrow database surface the PG repo needs (CONTRACT §17.2).
// Structurally identical to payment.PGPool so the main thread can share one
// *pgxpool.Pool across packages; the pool also satisfies queryer and txBeginner.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool and
// pgxmock expose on top of PGPool. The PGPool interface signature is frozen (no
// Query), so list methods type-assert the concrete pool to queryer; a pool
// lacking Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, q string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// txBeginner is the optional transaction capability used by ConsumeFIFOAtomic.
// Both *pgxpool.Pool and pgxmock.PgxPoolIface satisfy it; if a pool lacks it the
// consume call degrades to ErrNotFound (misconfigured DB) rather than panicking.
type txBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Compile-time guarantees that PGBatchRepo satisfies the frozen BatchRepo
// interface, the optional StockConsumer capability, the optional ProductExpirer
// capability (CONTRACT §17.5.1 / M7-A), and the optional BatchLister capability
// (BE-A, §24.2).
var (
	_ BatchRepo      = (*PGBatchRepo)(nil)
	_ StockConsumer  = (*PGBatchRepo)(nil)
	_ ProductExpirer = (*PGBatchRepo)(nil)
	_ BatchLister    = (*PGBatchRepo)(nil)
)

// SQL for inventory_batches (migration 000002). expiry_date / expiry_warn_date
// are nullable DATE; created_at / updated_at are defaulted by the DB and the
// trg_set_updated_at trigger refreshes updated_at on UPDATE.
const (
	sqlBatchCreate = `INSERT INTO inventory_batches (tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status, created_at, updated_at`
	sqlBatchGetByID = `SELECT id, tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status, created_at, updated_at
FROM inventory_batches
WHERE tenant_id = $1 AND id = $2`
	sqlBatchUpdate = `UPDATE inventory_batches
SET quantity = $1, in_price_cents = $2, expiry_date = $3, expiry_warn_date = $4, status = $5
WHERE tenant_id = $6 AND id = $7
RETURNING id, tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status, created_at, updated_at`
	sqlBatchListActiveFIFO = `SELECT id, tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status, created_at, updated_at
FROM inventory_batches
WHERE tenant_id = $1 AND store_id = $2 AND product_id = $3 AND status = 'active' AND quantity > 0
ORDER BY created_at ASC, id ASC`
	sqlBatchListNearing = `SELECT id, tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status, created_at, updated_at
FROM inventory_batches
WHERE tenant_id = $1 AND store_id = $2 AND status = 'active' AND quantity > 0
AND expiry_date IS NOT NULL AND expiry_date <= $3
ORDER BY id ASC`
	// ConsumeFIFOAtomic: lock the active lots oldest-first within the tx.
	sqlBatchLockFIFO = `SELECT id, tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status, created_at, updated_at
FROM inventory_batches
WHERE tenant_id = $1 AND store_id = $2 AND product_id = $3 AND status = 'active' AND quantity > 0
ORDER BY created_at ASC, id ASC
FOR UPDATE`
	// ConsumeFIFOAtomic: apply one lot's new quantity/status, addressed by tenant+id.
	sqlBatchConsumeUpdate = `UPDATE inventory_batches
SET quantity = $1, status = $2
WHERE tenant_id = $3 AND id = $4
RETURNING id, tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status, created_at, updated_at`
	// EarliestExpiryByProduct (M7-A): whole-tenant (storeID=0) or single-store
	// per-product earliest expiry. MIN(expiry_date) ignores NULLs, so lots without
	// an expiry never contribute; the GROUP BY collapses each product to its
	// soonest active lot. The ($2 = 0 OR store_id = $2) idiom keeps the SQL a
	// single constant (QueryMatcherEqual friendly) while letting storeID=0 span
	// all of the tenant's stores. tenant_id in the WHERE enforces row isolation.
	sqlBatchEarliestExpiryByProduct = `SELECT product_id, MIN(expiry_date)
FROM inventory_batches
WHERE tenant_id = $1 AND ($2 = 0 OR store_id = $2)
AND status = 'active' AND quantity > 0 AND expiry_date IS NOT NULL
GROUP BY product_id`
	// BatchLister (BE-A, §24.2): full-status paged listing for the boss 临期看板.
	// Optional filters reuse the ($N = 0 OR ...) / NULL-short-circuit idiom so the
	// SQL text stays a single constant (QueryMatcherEqual friendly): store/product
	// pass 0 when unset, status passes '' when unset, and expiry_before passes
	// NULL when unset (the $5::date IS NULL branch short-circuits the OR). Unlike
	// sqlBatchListActiveFIFO / sqlBatchListNearing this returns EVERY status
	// (active/nearing/expired/cleared) — the临期看板 only READS status. tenant_id
	// in the WHERE enforces row isolation.
	sqlBatchList = `SELECT id, tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status, created_at, updated_at
FROM inventory_batches
WHERE tenant_id = $1
AND ($2 = 0 OR store_id = $2)
AND ($3 = 0 OR product_id = $3)
AND ($4 = '' OR status = $4)
AND ($5::date IS NULL OR expiry_date <= $5)
ORDER BY id ASC
LIMIT $6 OFFSET $7`
	// sqlBatchListCount mirrors sqlBatchList's WHERE minus ORDER/LIMIT/OFFSET so
	// the pager can render the full total matching the SAME filter dimensions.
	sqlBatchListCount = `SELECT COUNT(*) FROM inventory_batches
WHERE tenant_id = $1
AND ($2 = 0 OR store_id = $2)
AND ($3 = 0 OR product_id = $3)
AND ($4 = '' OR status = $4)
AND ($5::date IS NULL OR expiry_date <= $5)`
)

// SQL for inventory_movements (migration 000002). batch_id / ref_order_id /
// operator_id are nullable BIGINT (0 -> NULL); note is nullable TEXT. The table
// is append-only with no updated_at, so there is no UPDATE path. created_at is
// caller-stamped (the service injects the clock), matching MemoryBatchRepo.
const (
	sqlMovementInsert = `INSERT INTO inventory_movements (tenant_id, store_id, product_id, batch_id, movement_type, quantity, ref_order_id, operator_id, note, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id`
	// Optional filters use the ($N = 0 OR ...) / NULL-short-circuit idiom so the
	// SQL text stays a single constant (QueryMatcherEqual friendly): store/product
	// pass 0 when unset, type passes '' when unset, and the day window passes
	// NULL for both bounds when unset (the IS NULL branch short-circuits the OR).
	sqlMovementList = `SELECT id, tenant_id, store_id, product_id, batch_id, movement_type, quantity, ref_order_id, operator_id, note, created_at
FROM inventory_movements
WHERE tenant_id = $1
AND ($2 = 0 OR store_id = $2)
AND ($3 = 0 OR product_id = $3)
AND ($4 = '' OR movement_type = $4)
AND ($5::timestamptz IS NULL OR (created_at >= $5 AND created_at < $6))
ORDER BY id ASC
LIMIT $7 OFFSET $8`
)

// ===== scan helpers =====

// scanBatch decodes one inventory_batches row. Nullable DATE columns
// (expiry_date / expiry_warn_date) come back through sql.NullTime; a valid
// value is re-pointed at a fresh time.Time so the caller cannot alias it.
func scanBatch(s rowScanner) (*Batch, error) {
	var b Batch
	var expiry, warn sql.NullTime
	if err := s.Scan(
		&b.ID, &b.TenantID, &b.StoreID, &b.ProductID, &b.Quantity, &b.InPriceCents,
		&expiry, &warn, &b.Status, &b.CreatedAt, &b.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if expiry.Valid {
		t := expiry.Time
		b.ExpiryDate = &t
	}
	if warn.Valid {
		t := warn.Time
		b.ExpiryWarnDate = &t
	}
	return &b, nil
}

// scanMovement decodes one inventory_movements row. Nullable BIGINT columns
// (batch_id / ref_order_id / operator_id) and note come back through sql.Null*
// types; a NULL maps to the zero value of the field (0 / ""), matching the
// domain convention "0 = none".
func scanMovement(s rowScanner) (*Movement, error) {
	var m Movement
	var batchID, refOrder, opID sql.NullInt64
	var note sql.NullString
	if err := s.Scan(
		&m.ID, &m.TenantID, &m.StoreID, &m.ProductID, &batchID, &m.Type,
		&m.Quantity, &refOrder, &opID, &note, &m.CreatedAt,
	); err != nil {
		return nil, err
	}
	m.BatchID = batchID.Int64
	m.RefOrderID = refOrder.Int64
	m.OperatorID = opID.Int64
	m.Note = note.String
	return &m, nil
}

// dateArg renders a *time.Time (nullable DATE) as a SQL arg: nil -> NULL
// (untyped nil, which pgx encodes as NULL), otherwise the dereferenced value.
func dateArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

// nullInt64 renders an int64 as a nullable BIGINT arg: 0 -> NULL, otherwise the
// value. Used for batch_id / ref_order_id / operator_id ("0 = none" -> NULL).
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// movementInsertArgs builds the ordered argument slice for sqlMovementInsert,
// shared by AppendMovement and the ConsumeFIFOAtomic ledger writes so the two
// paths cannot drift.
func movementInsertArgs(m *Movement) []any {
	return []any{
		m.TenantID, m.StoreID, m.ProductID,
		nullInt64(m.BatchID), m.Type, m.Quantity,
		nullInt64(m.RefOrderID), nullInt64(m.OperatorID), m.Note, m.CreatedAt,
	}
}

// ===== PGBatchRepo =====

// PGBatchRepo is the pgx-backed BatchRepo. A nil pool makes reads return
// ErrNotFound (single) / empty slice (list) and writes return ErrNotFound, so a
// misconfigured main thread degrades gracefully instead of panicking.
type PGBatchRepo struct{ pool PGPool }

// NewPGBatchRepo constructs a PGBatchRepo over pool.
func NewPGBatchRepo(pool PGPool) *PGBatchRepo { return &PGBatchRepo{pool: pool} }

// CreateBatch inserts b and returns the persisted record (id/timestamps stamped
// by the DB via RETURNING). A nil pool or nil batch degrades to ErrNotFound.
func (r *PGBatchRepo) CreateBatch(ctx context.Context, b *Batch) (*Batch, error) {
	if r == nil || r.pool == nil || b == nil {
		return nil, ErrNotFound
	}
	created, err := scanBatch(r.pool.QueryRow(ctx, sqlBatchCreate,
		b.TenantID, b.StoreID, b.ProductID, b.Quantity, b.InPriceCents,
		dateArg(b.ExpiryDate), dateArg(b.ExpiryWarnDate), b.Status,
	))
	if err != nil {
		return nil, fmt.Errorf("inventory: pg batch create: %w", err)
	}
	return cloneBatch(created), nil
}

// GetBatch loads the batch for (tenantID, id); ErrNotFound when absent or
// cross-tenant (the WHERE tenant_id makes the two indistinguishable, by design).
func (r *PGBatchRepo) GetBatch(ctx context.Context, tenantID, id int64) (*Batch, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	b, err := scanBatch(r.pool.QueryRow(ctx, sqlBatchGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("inventory: pg batch get: %w", err)
	}
	return cloneBatch(b), nil
}

// UpdateBatch persists the mutable stock fields (quantity / in_price_cents /
// expiry / status) addressed by ID within b.TenantID. store_id / product_id are
// treated as immutable identity (the service only mutates quantity/status).
// ErrNotFound when the lot is absent or cross-tenant (RETURNING yields no row).
func (r *PGBatchRepo) UpdateBatch(ctx context.Context, b *Batch) (*Batch, error) {
	if r == nil || r.pool == nil || b == nil {
		return nil, ErrNotFound
	}
	updated, err := scanBatch(r.pool.QueryRow(ctx, sqlBatchUpdate,
		b.Quantity, b.InPriceCents, dateArg(b.ExpiryDate), dateArg(b.ExpiryWarnDate), b.Status,
		b.TenantID, b.ID,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("inventory: pg batch update: %w", err)
	}
	return cloneBatch(updated), nil
}

// ListActiveBatchesFIFO returns the active, non-empty lots of one product within
// (tenantID, storeID), oldest first (created_at ASC, id ASC). A pool lacking
// Query (or nil) yields an empty slice.
func (r *PGBatchRepo) ListActiveBatchesFIFO(ctx context.Context, tenantID, storeID, productID int64) ([]*Batch, error) {
	out := make([]*Batch, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlBatchListActiveFIFO, tenantID, storeID, productID)
	if err != nil {
		return nil, fmt.Errorf("inventory: pg batch list fifo: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("inventory: pg batch list fifo scan: %w", err)
		}
		out = append(out, cloneBatch(b))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inventory: pg batch list fifo rows: %w", err)
	}
	return out, nil
}

// ListNearingBatches returns the active, positive-quantity lots of
// (tenantID, storeID) whose ExpiryDate is set and falls on or before
// now()+withinDays (the same deadline MemoryBatchRepo computes, via the shared
// package clock so tests stay deterministic). Lots without an ExpiryDate are
// excluded. Ordered by id ASC for deterministic output.
func (r *PGBatchRepo) ListNearingBatches(ctx context.Context, tenantID, storeID int64, withinDays int) ([]*Batch, error) {
	out := make([]*Batch, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	deadline := now().AddDate(0, 0, withinDays)
	rows, err := q.Query(ctx, sqlBatchListNearing, tenantID, storeID, deadline)
	if err != nil {
		return nil, fmt.Errorf("inventory: pg batch list nearing: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("inventory: pg batch list nearing scan: %w", err)
		}
		out = append(out, cloneBatch(b))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inventory: pg batch list nearing rows: %w", err)
	}
	return out, nil
}

// EarliestExpiryByProduct implements the optional ProductExpirer capability
// (M7-A): for tenantID it returns each product's earliest (soonest) expiry_date
// across its active, positive-quantity lots via MIN(expiry_date) GROUP BY
// product_id. storeID=0 spans all of the tenant's stores (the ($2 = 0 OR
// store_id = $2) idiom); storeID>0 scopes to one store. Lots without an expiry
// are excluded (MIN ignores NULLs + the IS NOT NULL filter). tenant_id in the
// WHERE enforces row isolation. A nil/non-Query pool yields an empty map.
func (r *PGBatchRepo) EarliestExpiryByProduct(ctx context.Context, tenantID, storeID int64) (map[int64]time.Time, error) {
	out := make(map[int64]time.Time)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlBatchEarliestExpiryByProduct, tenantID, storeID)
	if err != nil {
		return nil, fmt.Errorf("inventory: pg earliest expiry: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var productID int64
		var expiry time.Time
		if err := rows.Scan(&productID, &expiry); err != nil {
			return nil, fmt.Errorf("inventory: pg earliest expiry scan: %w", err)
		}
		out[productID] = expiry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inventory: pg earliest expiry rows: %w", err)
	}
	return out, nil
}

// ListBatches implements the optional BatchLister capability (BE-A, §24.2). It
// returns the ctx tenant's lots matching f (store/product/status/expiry_before)
// ordered by id ASC with Limit/Offset applied, covering EVERY status
// (active/nearing/expired/cleared) so the boss 临期看板 sees cleared/expired lots
// too — it only READS status (nearing/expired transitions stay owned by the
// BE-A-09 tag job). Optional filters use the same sentinel-arg idiom as
// sqlMovementList. A nil/non-Query pool yields an empty slice.
func (r *PGBatchRepo) ListBatches(ctx context.Context, tenantID int64, f BatchFilter) ([]*Batch, error) {
	out := make([]*Batch, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultBatchListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	rows, err := q.Query(ctx, sqlBatchList,
		tenantID, f.StoreID, f.ProductID, f.Status, dateArg(f.ExpiryBefore), limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("inventory: pg batch list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, fmt.Errorf("inventory: pg batch list scan: %w", err)
		}
		out = append(out, cloneBatch(b))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inventory: pg batch list rows: %w", err)
	}
	return out, nil
}

// ListBatchesCount implements the optional BatchLister capability (BE-A, §24.2).
// It returns the total count of the ctx tenant's batches matching f WITHOUT
// Limit/Offset (same filter dimensions as ListBatches), so the pager can render
// the full total. A nil/non-Query pool yields 0.
func (r *PGBatchRepo) ListBatchesCount(ctx context.Context, tenantID int64, f BatchFilter) (int64, error) {
	if r == nil || r.pool == nil {
		return 0, nil
	}
	// COUNT(*) goes through QueryRow (the frozen PGPool surface), so no queryer
	// type-assert is needed; a nil pool short-circuits above.
	var n int64
	if err := r.pool.QueryRow(ctx, sqlBatchListCount,
		tenantID, f.StoreID, f.ProductID, f.Status, dateArg(f.ExpiryBefore),
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("inventory: pg batch list count: %w", err)
	}
	return n, nil
}

// AppendMovement appends an immutable ledger entry and returns a defensive copy
// with the allocated id. Tenant/store/product/type come from the caller (the
// ConsumeFIFOAtomic path stamps them from the consumed lot). A nil pool degrades
// to ErrNotFound (the movement write is not idempotent, so a silent no-op with a
// zero id would lose an audit line — ErrNotFound surfaces the misconfiguration).
func (r *PGBatchRepo) AppendMovement(ctx context.Context, m *Movement) (*Movement, error) {
	if r == nil || r.pool == nil || m == nil {
		return nil, ErrNotFound
	}
	stored := cloneMovement(m)
	if err := r.pool.QueryRow(ctx, sqlMovementInsert, movementInsertArgs(stored)...).Scan(&stored.ID); err != nil {
		return nil, fmt.Errorf("inventory: pg movement append: %w", err)
	}
	return cloneMovement(stored), nil
}

// ListMovements returns ledger entries within tenantID matching f. Optional
// filters (store/product/type/day) are encoded as sentinel args against the
// single static SQL; limit<=0 defaults to defaultMovementLimit. Day filters by
// the natural calendar day in f.Day's own location. A nil/non-Query pool yields
// an empty slice.
func (r *PGBatchRepo) ListMovements(ctx context.Context, tenantID int64, f MovementFilter) ([]*Movement, error) {
	out := make([]*Movement, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultMovementLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	// Natural-day window in Day's own location (mirrors MemoryBatchRepo).
	var dayStart, dayEnd any
	if f.Day != nil {
		y, mo, d := f.Day.Date()
		loc := f.Day.Location()
		start := time.Date(y, mo, d, 0, 0, 0, 0, loc)
		dayStart, dayEnd = start, start.AddDate(0, 0, 1)
	}
	rows, err := q.Query(ctx, sqlMovementList,
		tenantID, f.StoreID, f.ProductID, f.Type, dayStart, dayEnd, limit, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("inventory: pg movement list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMovement(rows)
		if err != nil {
			return nil, fmt.Errorf("inventory: pg movement list scan: %w", err)
		}
		out = append(out, cloneMovement(m))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inventory: pg movement list rows: %w", err)
	}
	return out, nil
}

// ConsumeFIFOAtomic implements the optional StockConsumer capability as a single
// SELECT ... FOR UPDATE transaction: the active lots are locked oldest-first,
// the deduction is planned in Go, and on full coverage each touched lot is
// UPDATEd (quantity/status) with one sale_out movement appended per lot — all
// committed atomically. Insufficient stock rolls back and returns
// ErrInsufficientStock having applied NOTHING. mkMovement is invoked once per
// touched lot to build its ledger line; the repo stamps tenant/store/product/
// type defensively from the consume scope (matching MemoryBatchRepo).
//
// inventory deliberately does NOT import pkg/db, so the transaction is driven
// through a local txBeginner type-assert over the pool. Both *pgxpool.Pool and
// pgxmock satisfy it; a pool lacking BeginTx degrades to ErrNotFound.
func (r *PGBatchRepo) ConsumeFIFOAtomic(
	ctx context.Context,
	tenantID, storeID, productID, qty int64,
	mkMovement func(batchID, deducted int64) *Movement,
) ([]*Batch, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	beginner, ok := r.pool.(txBeginner)
	if !ok {
		return nil, ErrNotFound
	}
	tx, err := beginner.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("inventory: pg consume fifo begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Lock the active lots oldest-first (FOR UPDATE prevents concurrent oversell).
	rows, err := tx.Query(ctx, sqlBatchLockFIFO, tenantID, storeID, productID)
	if err != nil {
		return nil, fmt.Errorf("inventory: pg consume fifo select: %w", err)
	}
	active := make([]*Batch, 0, 4)
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("inventory: pg consume fifo scan: %w", err)
		}
		active = append(active, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("inventory: pg consume fifo rows: %w", err)
	}

	// Dry-run the plan against the locked rows; verify full coverage before any
	// mutation (no partial deduction is ever observable).
	type plan struct {
		batch  *Batch
		deduct int64
	}
	plans := make([]plan, 0, len(active))
	remaining := qty
	for _, b := range active {
		if remaining == 0 {
			break
		}
		d := b.Quantity
		if d > remaining {
			d = remaining
		}
		plans = append(plans, plan{batch: b, deduct: d})
		remaining -= d
	}
	if remaining > 0 {
		// Deferred Rollback runs; nothing was written.
		return nil, ErrInsufficientStock
	}

	// Coverage confirmed: apply each lot's new quantity/status and append its
	// sale_out movement within the same transaction.
	updated := make([]*Batch, 0, len(plans))
	for _, p := range plans {
		newQty := p.batch.Quantity - p.deduct
		status := p.batch.Status
		if newQty == 0 {
			status = BatchStatusCleared
		}
		ub, err := scanBatch(tx.QueryRow(ctx, sqlBatchConsumeUpdate, newQty, status, tenantID, p.batch.ID))
		if err != nil {
			return nil, fmt.Errorf("inventory: pg consume fifo update: %w", err)
		}
		updated = append(updated, cloneBatch(ub))

		if mkMovement != nil {
			mv := mkMovement(p.batch.ID, p.deduct)
			if mv != nil {
				mv.TenantID = tenantID
				mv.StoreID = storeID
				mv.ProductID = productID
				mv.Type = MovementTypeSaleOut
				if err := tx.QueryRow(ctx, sqlMovementInsert, movementInsertArgs(mv)...).Scan(&mv.ID); err != nil {
					return nil, fmt.Errorf("inventory: pg consume fifo movement: %w", err)
				}
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("inventory: pg consume fifo commit: %w", err)
	}
	committed = true
	return updated, nil
}
