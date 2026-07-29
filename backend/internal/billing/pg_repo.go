package billing

// pg_repo.go implements the pgx-backed OrderRepo (CONTRACT §17.4: billing,
// migration 000003 orders/order_items). It follows the payment/pg_repo.go idiom
// verbatim: a narrow PGPool surface (QueryRow / Exec), a queryer type-assertion
// for the multi-row reads (PGPool deliberately omits Query so the frozen
// interface stays minimal), scanXxx helpers over rowScanner, nil-pool safe
// degradation to ErrNotFound / empty slice / zero, and pgx.ErrNoRows mapped to
// billing.ErrNotFound. Tenant isolation lives in every statement's
// WHERE tenant_id = $N (ARCH-01).
//
// Cross-table writes (Create) use payment's plain QueryRow+Exec multi-statement
// style: INSERT the order with RETURNING id/timestamps, then loop-insert every
// OrderItem carrying the allocated order id. Multi-row reads (ListByDate /
// ListByMember) query orders then load each order's items (N+1 over the
// order_items index), the same SELECT-then-SELECT shape the contract endorses
// for GetByID. Aggregate reads (OrderTotalByMembers / RepurchaseMembers) scan a
// single COALESCE(SUM/COUNT) column.
//
// UpdateStatus for the paid target mirrors payment.MarkSuccess: read current,
// then a conditional pending->paid UPDATE (idempotent on already-paid,
// ErrStatusConflict past paid). Non-paid targets are a plain status UPDATE that
// leaves the payment fields untouched, matching the memory repo.

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
// *pgxpool.Pool across packages (it also satisfies queryer).
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// queryer is the optional multi-row read surface that the real *pgxpool.Pool
// and pgxmock expose on top of PGPool. The PGPool interface signature is frozen
// (no Query), so list/load methods type-assert the concrete pool to queryer; a
// pool lacking Query returns an empty slice rather than failing.
type queryer interface {
	Query(ctx context.Context, q string, args ...any) (pgx.Rows, error)
}

// rowScanner is the common Scan surface of pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// SQL for orders (migration 000003). The order column list is shared by every
// SELECT and RETURNING clause so scanOrder decodes them uniformly.
//
// NOTE (schema gap): the Order struct carries PaymentRef (added M4-S1,
// load-bearing for MarkOrderPaid's pending->paid promotion), but migration
// 000003 predates it and no later migration adds orders.payment_ref. This repo
// persists payment_ref as a column so the PG implementation is production-ready
// and behaviourally identical to MemoryOrderRepo (CONTRACT §17.1: "配置 PG 后
// 可直接切换"). The main thread MUST add a migration adding
// `ALTER TABLE orders ADD COLUMN payment_ref TEXT` before the memory->pg
// cutover; pgxmock does not validate schema, so the gap is invisible to tests.
const (
	orderSelectCols = `id, tenant_id, store_id, member_id, order_no, channel, status, total_cents, paid_cents, discount_cents, payment_method, operator_id, paid_at, payment_ref, created_at, updated_at`

	sqlOrderCreate = `INSERT INTO orders (tenant_id, store_id, member_id, order_no, channel, status, total_cents, paid_cents, discount_cents, payment_method, operator_id, paid_at, payment_ref, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING id, created_at, updated_at`

	sqlOrderItemCreate = `INSERT INTO order_items (tenant_id, order_id, product_id, name_snapshot, unit, qty, price_cents, cost_cents) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

	sqlOrderGetByID = `SELECT ` + orderSelectCols + ` FROM orders WHERE tenant_id = $1 AND id = $2`

	sqlOrdersByDateStore = `SELECT ` + orderSelectCols + ` FROM orders WHERE tenant_id = $1 AND store_id = $2 AND created_at >= $3 AND created_at < $4 ORDER BY id`
	sqlOrdersByDate      = `SELECT ` + orderSelectCols + ` FROM orders WHERE tenant_id = $1 AND created_at >= $2 AND created_at < $3 ORDER BY id`

	// sqlOrdersByMember scopes by tenant + member. Walk-in orders are stored
	// with member_id NULL (see nullInt64), so member_id = $2 never matches a
	// walk-in even if caller passes 0 — matching "散客单永不返回".
	sqlOrdersByMember = `SELECT ` + orderSelectCols + ` FROM orders WHERE tenant_id = $1 AND member_id = $2 ORDER BY created_at DESC, id DESC`

	sqlOrderItemsByOrder = `SELECT product_id, name_snapshot, unit, qty, price_cents, cost_cents FROM order_items WHERE tenant_id = $1 AND order_id = $2 ORDER BY id`

	// Aggregates: member_id = ANY($2) accepts a Go []int64 directly (pgx
	// encodes it); NULL member_id rows never match, so walk-ins are excluded.
	sqlOrderTotalByMembers = `SELECT COALESCE(SUM(total_cents), 0) FROM orders WHERE tenant_id = $1 AND status = 'paid' AND member_id = ANY($2) AND paid_at BETWEEN $3 AND $4`
	sqlRepurchaseMembers   = `SELECT COUNT(DISTINCT member_id) FROM orders WHERE tenant_id = $1 AND status = 'paid' AND member_id = ANY($2) AND paid_at BETWEEN $3 AND $4`

	// Non-paid status update: status only, payment fields untouched (the
	// trg_set_updated_at trigger refreshes updated_at on UPDATE).
	sqlOrderUpdateStatus = `UPDATE orders SET status = $1 WHERE tenant_id = $2 AND id = $3 RETURNING ` + orderSelectCols

	// Pending->paid promotion (BE-A-10). Conditional on status='pending' so a
	// race that flips the row out of pending between the read and the update
	// surfaces as no rows -> ErrStatusConflict (same idiom as payment.MarkSuccess).
	// updated_at tracks paid_at when present, else now().
	sqlOrderMarkPaid = `UPDATE orders SET status = 'paid', paid_cents = $1, paid_at = $2, payment_ref = $3, updated_at = COALESCE($2, now()) WHERE tenant_id = $4 AND id = $5 AND status = 'pending' RETURNING ` + orderSelectCols
)

// ===== scan helpers =====

// scanOrder decodes one orders row. Nullable columns (member_id /
// payment_method / operator_id / paid_at / payment_ref) scan into database/sql
// Null types; member_id NULL <-> MemberID 0 (walk-in).
func scanOrder(s rowScanner) (*Order, error) {
	var o Order
	var memberID sql.NullInt64
	var paymentMethod sql.NullString
	var operatorID sql.NullInt64
	var paidAt sql.NullTime
	var paymentRef sql.NullString
	if err := s.Scan(
		&o.ID, &o.TenantID, &o.StoreID, &memberID, &o.OrderNo, &o.Channel, &o.Status,
		&o.TotalCents, &o.PaidCents, &o.DiscountCents, &paymentMethod, &operatorID,
		&paidAt, &paymentRef, &o.CreatedAt, &o.UpdatedAt,
	); err != nil {
		return nil, err
	}
	o.MemberID = memberID.Int64
	o.PaymentMethod = paymentMethod.String
	o.OperatorID = operatorID.Int64
	if paidAt.Valid {
		t := paidAt.Time
		o.PaidAt = &t
	}
	o.PaymentRef = paymentRef.String
	return &o, nil
}

// scanOrderItem decodes one order_items row (the six OrderItem fields; the
// table's id/batch_id/created_at are not part of the domain model).
func scanOrderItem(s rowScanner) (OrderItem, error) {
	var it OrderItem
	if err := s.Scan(&it.ProductID, &it.NameSnapshot, &it.Unit, &it.Qty, &it.PriceCents, &it.CostCents); err != nil {
		return it, err
	}
	return it, nil
}

// nullInt64 maps a domain int64 to sql.NullInt64; 0 (sentinel for "none") ->
// NULL, matching the nullable member_id / operator_id columns.
func nullInt64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

// nullStr maps a domain string to sql.NullString; "" -> NULL, matching the
// nullable payment_method / payment_ref columns.
func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// nullTimePtr maps a domain *time.Time to sql.NullTime; nil -> NULL.
func nullTimePtr(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

// ===== PGOrderRepo =====

// PGOrderRepo is the pgx-backed OrderRepo. A nil pool makes reads return
// ErrNotFound / empty slice / 0 and writes return ErrNotFound, so a
// misconfigured main thread degrades gracefully rather than panicking.
type PGOrderRepo struct{ pool PGPool }

// NewPGOrderRepo constructs a PGOrderRepo over pool.
func NewPGOrderRepo(pool PGPool) *PGOrderRepo { return &PGOrderRepo{pool: pool} }

// Compile-time guarantee that PGOrderRepo satisfies the full OrderRepo
// interface (all 7 methods, CONTRACT §17.5 item 1: "接口冻结").
var _ OrderRepo = (*PGOrderRepo)(nil)

// loadItems fetches the OrderItems for (tenantID, orderID) ordered by id so the
// list mirrors insertion order. A pool without Query yields an empty slice.
func (r *PGOrderRepo) loadItems(ctx context.Context, tenantID, orderID int64) ([]OrderItem, error) {
	items := make([]OrderItem, 0)
	if r == nil || r.pool == nil {
		return items, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return items, nil
	}
	rows, err := q.Query(ctx, sqlOrderItemsByOrder, tenantID, orderID)
	if err != nil {
		return nil, fmt.Errorf("billing: pg order items load: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanOrderItem(rows)
		if err != nil {
			return nil, fmt.Errorf("billing: pg order items scan: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing: pg order items rows: %w", err)
	}
	return items, nil
}

// Create persists the order together with its Items and returns the persisted
// record. Cross-table write: INSERT order RETURNING id/timestamps, then loop
// INSERT each item carrying the allocated order id (payment's plain
// QueryRow+Exec style — no transaction at this layer; the main thread may wrap
// the service call if it needs atomicity).
func (r *PGOrderRepo) Create(ctx context.Context, o *Order) (*Order, error) {
	if r == nil || r.pool == nil || o == nil {
		return nil, ErrNotFound
	}
	if err := r.pool.QueryRow(ctx, sqlOrderCreate,
		o.TenantID, o.StoreID, nullInt64(o.MemberID), o.OrderNo, o.Channel, o.Status,
		o.TotalCents, o.PaidCents, o.DiscountCents, nullStr(o.PaymentMethod), nullInt64(o.OperatorID),
		nullTimePtr(o.PaidAt), nullStr(o.PaymentRef), o.CreatedAt, o.UpdatedAt,
	).Scan(&o.ID, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, fmt.Errorf("billing: pg order create: %w", err)
	}
	for _, it := range o.Items {
		if _, err := r.pool.Exec(ctx, sqlOrderItemCreate,
			o.TenantID, o.ID, it.ProductID, it.NameSnapshot, it.Unit, it.Qty, it.PriceCents, it.CostCents,
		); err != nil {
			return nil, fmt.Errorf("billing: pg order item create: %w", err)
		}
	}
	return cloneOrder(o), nil
}

// GetByID loads the order (with Items) for (tenantID, id); ErrNotFound when
// absent or owned by another tenant (cross-tenant reads are indistinguishable
// from absence — ARCH-01).
func (r *PGOrderRepo) GetByID(ctx context.Context, tenantID, id int64) (*Order, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	o, err := scanOrder(r.pool.QueryRow(ctx, sqlOrderGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("billing: pg order get: %w", err)
	}
	items, err := r.loadItems(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	o.Items = items
	return o, nil
}

// ListByDate returns the tenant's orders for storeID created on the natural
// calendar day containing day (day's own location, matching MemoryOrderRepo's
// semantics for memory<->pg drop-in equivalence). storeID<=0 means all stores.
// Each order carries its Items.
func (r *PGOrderRepo) ListByDate(ctx context.Context, tenantID, storeID int64, day time.Time) ([]*Order, error) {
	out := make([]*Order, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	y, m, d := day.Date()
	loc := day.Location()
	start := time.Date(y, m, d, 0, 0, 0, 0, loc)
	next := start.AddDate(0, 0, 1)

	var rows pgx.Rows
	var err error
	if storeID > 0 {
		rows, err = q.Query(ctx, sqlOrdersByDateStore, tenantID, storeID, start, next)
	} else {
		rows, err = q.Query(ctx, sqlOrdersByDate, tenantID, start, next)
	}
	if err != nil {
		return nil, fmt.Errorf("billing: pg orders by date: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("billing: pg orders by date scan: %w", err)
		}
		items, err := r.loadItems(ctx, tenantID, o.ID)
		if err != nil {
			return nil, err
		}
		o.Items = items
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing: pg orders by date rows: %w", err)
	}
	return out, nil
}

// ListByMember returns ALL orders (with Items) owned by memberID within
// tenantID, ordered by created_at DESC then id DESC (newest first). A walk-in
// order (member_id NULL) is never matched. An empty result returns a non-nil
// empty slice. This backs the C-side "我的订单" list (CONTRACT §16.3(7)/§16.8).
func (r *PGOrderRepo) ListByMember(ctx context.Context, tenantID, memberID int64) ([]*Order, error) {
	out := make([]*Order, 0)
	if r == nil || r.pool == nil {
		return out, nil
	}
	q, ok := r.pool.(queryer)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlOrdersByMember, tenantID, memberID)
	if err != nil {
		return nil, fmt.Errorf("billing: pg orders by member: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("billing: pg orders by member scan: %w", err)
		}
		items, err := r.loadItems(ctx, tenantID, o.ID)
		if err != nil {
			return nil, err
		}
		o.Items = items
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("billing: pg orders by member rows: %w", err)
	}
	return out, nil
}

// OrderTotalByMembers sums total_cents of the tenant's PAID orders whose
// member_id is in memberIDs and whose paid_at falls in [since, until]
// (inclusive). Empty memberIDs or since.After(until) returns 0,nil. Cross-tenant
// orders are excluded by tenantID.
func (r *PGOrderRepo) OrderTotalByMembers(ctx context.Context, tenantID int64, memberIDs []int64, since, until time.Time) (int64, error) {
	if r == nil || r.pool == nil {
		return 0, nil
	}
	if len(memberIDs) == 0 || since.After(until) {
		return 0, nil
	}
	var sum int64
	if err := r.pool.QueryRow(ctx, sqlOrderTotalByMembers, tenantID, memberIDs, since, until).Scan(&sum); err != nil {
		return 0, fmt.Errorf("billing: pg order total by members: %w", err)
	}
	return sum, nil
}

// RepurchaseMembers returns the count of DISTINCT members in memberIDs with at
// least one PAID order in [since, until] (inclusive) within tenantID. Empty
// memberIDs or since.After(until) returns 0,nil.
func (r *PGOrderRepo) RepurchaseMembers(ctx context.Context, tenantID int64, memberIDs []int64, since, until time.Time) (int, error) {
	if r == nil || r.pool == nil {
		return 0, nil
	}
	if len(memberIDs) == 0 || since.After(until) {
		return 0, nil
	}
	var count int64
	if err := r.pool.QueryRow(ctx, sqlRepurchaseMembers, tenantID, memberIDs, since, until).Scan(&count); err != nil {
		return 0, fmt.Errorf("billing: pg repurchase members: %w", err)
	}
	return int(count), nil
}

// UpdateStatus transitions an order's status. When the target is "paid" it fills
// paid_cents / paid_at / payment_ref following the billing state machine
// (mirror of payment.MarkSuccess): pending->paid performs the conditional UPDATE
// and returns the updated order; already-paid is idempotent (returns current,
// no write, no PaymentRef overwrite — duplicate notifies must not double-pay);
// any other source state is ErrStatusConflict. For a non-paid target the payment
// fields are left untouched (callers pass zero values) and a plain status UPDATE
// runs. Cross-tenant or absent id -> ErrNotFound. Returns the updated order with
// Items (CONTRACT §15.5.5 / BE-A-10).
func (r *PGOrderRepo) UpdateStatus(ctx context.Context, tenantID, id int64, status string, paidCents int64, paidAt *time.Time, paymentRef string) (*Order, error) {
	if r == nil || r.pool == nil {
		return nil, ErrNotFound
	}
	current, err := scanOrder(r.pool.QueryRow(ctx, sqlOrderGetByID, tenantID, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("billing: pg update status load: %w", err)
	}
	items, err := r.loadItems(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}

	if status != orderStatusPaid {
		// Non-paid target: update status only, payment fields untouched.
		updated, err := scanOrder(r.pool.QueryRow(ctx, sqlOrderUpdateStatus, status, tenantID, id))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("billing: pg update status: %w", err)
		}
		updated.Items = items
		return updated, nil
	}

	// target == paid: state machine.
	switch current.Status {
	case orderStatusPaid:
		// Idempotent: already paid — return current unchanged so a duplicate
		// notify does not overwrite PaymentRef / PaidAt or re-deduct stock.
		current.Items = items
		return current, nil
	case orderStatusPending:
		updated, err := scanOrder(r.pool.QueryRow(ctx, sqlOrderMarkPaid,
			paidCents, nullTimePtr(paidAt), nullStr(paymentRef), tenantID, id,
		))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Raced: another writer flipped it out of pending between the
				// read and the conditional update. Treat as conflict.
				return nil, ErrStatusConflict
			}
			return nil, fmt.Errorf("billing: pg mark paid: %w", err)
		}
		updated.Items = items
		return updated, nil
	default:
		// refunded / voided / unknown: the order has moved past the payable
		// state; paying it again is a status conflict.
		return nil, ErrStatusConflict
	}
}
