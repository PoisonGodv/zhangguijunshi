package subscription

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zhangguijunshi/backend/pkg/errcode"
)

// SubscriptionStore persists subscription state. MemoryStore for dev/test,
// PGStore for production (CONTRACT §15.3.2 / §15.5.7). Every method is scoped
// by tenantID: a row written under tenant A is invisible to tenant B (ARCH-01).
type SubscriptionStore interface {
	// CurrentPlan returns the tenant's current active plan; no record (or
	// non-active status) → free. Never errors for "no subscription".
	CurrentPlan(ctx context.Context, tenantID int64) (string, error)
	// Get returns the current active subscription record, or nil if none.
	Get(ctx context.Context, tenantID int64) (*Subscription, error)
	// Upsert activates/renews: prior active rows are set cancelled, a new
	// active row is created. Returns the new record.
	Upsert(ctx context.Context, sub *Subscription) (*Subscription, error)
	// ListHistory returns the subscription audit trail (all statuses).
	ListHistory(ctx context.Context, tenantID int64) ([]*Subscription, error)
	// ExpireDue flips every active subscription whose period_ends_at has passed
	// to expired (CONTRACT §15.8). It is a GLOBAL batch operation (all tenants)
	// run by the cron binary — it does NOT take a tenant. Returns the number of
	// rows transitioned. Subscriptions with nil period_ends_at (永久/free) are
	// never expired.
	ExpireDue(ctx context.Context, now time.Time) (int, error)
}

// MemoryStore is the in-memory dev/test SubscriptionStore. It is safe for
// concurrent use (sync.Mutex over a per-tenant slice). Each Upsert assigns a
// monotonic ID; auto-expiry (period_ends_at → expired) is deferred to a future
// cron (CONTRACT §15.8), so CurrentPlan/Get read status=active only.
type MemoryStore struct {
	mu       sync.Mutex
	nextID   int64
	byTenant map[int64][]*Subscription
}

// NewMemoryStore constructs an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byTenant: make(map[int64][]*Subscription)}
}

// CurrentPlan returns the tenant's active plan, or free when there is none.
func (s *MemoryStore) CurrentPlan(_ context.Context, tenantID int64) (string, error) {
	if sub := s.active(tenantID); sub != nil {
		return sub.Plan, nil
	}
	return PlanFree, nil
}

// Get returns a copy of the tenant's active subscription, or nil.
func (s *MemoryStore) Get(_ context.Context, tenantID int64) (*Subscription, error) {
	if sub := s.active(tenantID); sub != nil {
		cp := *sub
		return &cp, nil
	}
	return nil, nil
}

// active returns the tenant's active record under the mutex (nil if none).
func (s *MemoryStore) active(tenantID int64) *Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.byTenant[tenantID] {
		if sub.Status == "active" {
			return sub
		}
	}
	return nil
}

// Upsert cancels prior active rows for the tenant, then appends a new active
// record (UNIQUE active one invariant). ID is assigned monotonically; the
// caller-supplied timestamps (CreatedAt/UpdatedAt) are honored so tests stay
// deterministic (no time.Now here). Returns a copy of the new record.
func (s *MemoryStore) Upsert(_ context.Context, sub *Subscription) (*Subscription, error) {
	if sub == nil {
		return nil, errcode.ErrParam
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// mutation timestamp for cancelling prior rows: caller-provided now.
	mark := sub.UpdatedAt
	if mark.IsZero() {
		mark = sub.CreatedAt
	}
	for _, old := range s.byTenant[sub.TenantID] {
		if old.Status == "active" {
			old.Status = "cancelled"
			if !mark.IsZero() {
				old.UpdatedAt = mark
			}
		}
	}
	s.nextID++
	rec := *sub
	rec.ID = s.nextID
	if rec.Status == "" {
		rec.Status = "active"
	}
	if rec.Plan == "" {
		rec.Plan = PlanFree
	}
	s.byTenant[sub.TenantID] = append(s.byTenant[sub.TenantID], &rec)
	out := rec
	return &out, nil
}

// ListHistory returns copies of every subscription record for the tenant
// (insertion/ID order). An unknown tenant yields an empty (non-nil) slice.
func (s *MemoryStore) ListHistory(_ context.Context, tenantID int64) ([]*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.byTenant[tenantID]
	out := make([]*Subscription, 0, len(src))
	for _, sub := range src {
		cp := *sub
		out = append(out, &cp)
	}
	return out, nil
}

// ExpireDue transitions every active subscription whose period has ended to
// expired (CONTRACT §15.8). Global across tenants; nil period_ends_at (永久)
// is never expired. Returns the count of transitioned rows.
func (s *MemoryStore) ExpireDue(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, subs := range s.byTenant {
		for _, sub := range subs {
			if sub.Status == "active" && sub.PeriodEndsAt != nil && sub.PeriodEndsAt.Before(now) {
				sub.Status = "expired"
				sub.UpdatedAt = now
				n++
			}
		}
	}
	return n, nil
}

// ===== PG production store (pgxmock tested) =====

// PGPool is the narrow DB surface PGStore needs for single-row reads/writes
// (mirrors attribution.PGPool / bandit.PGRepo, CONTRACT §15.5.7).
// ListHistory additionally type-asserts the pool to pgQuerier for multi-row
// reads so the frozen PGPool surface stays minimal.
type PGPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// pgQuerier is the multi-row read surface both *pgxpool.Pool and
// pgxmock.PgxPoolIface satisfy; a pool that only satisfies PGPool yields an
// empty result rather than a panic.
type pgQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// SQL statements against subscriptions (migration 000013).
const (
	sqlSubCurrentPlan = `SELECT plan FROM subscriptions WHERE tenant_id = $1 AND status = 'active' LIMIT 1`

	sqlSubGet = `SELECT id, tenant_id, plan, period_started_at, period_ends_at, status, source, payment_ref, created_at, updated_at
FROM subscriptions WHERE tenant_id = $1 AND status = 'active' LIMIT 1`

	// sqlSubCancelActive flips the tenant's prior active row(rows) to
	// cancelled before the new active insert (UNIQUE active one invariant).
	sqlSubCancelActive = `UPDATE subscriptions SET status = 'cancelled' WHERE tenant_id = $1 AND status = 'active'`

	sqlSubInsert = `INSERT INTO subscriptions (tenant_id, plan, period_started_at, period_ends_at, status, source, payment_ref, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, tenant_id, plan, period_started_at, period_ends_at, status, source, payment_ref, created_at, updated_at`

	sqlSubListHistory = `SELECT id, tenant_id, plan, period_started_at, period_ends_at, status, source, payment_ref, created_at, updated_at
FROM subscriptions WHERE tenant_id = $1 ORDER BY id`

	// sqlSubExpireDue flips every active subscription past its period end to
	// expired (CONTRACT §15.8). Global (all tenants); nil period_ends_at never
	// matches. $1 = now.
	sqlSubExpireDue = `UPDATE subscriptions SET status = 'expired', updated_at = $1
WHERE status = 'active' AND period_ends_at IS NOT NULL AND period_ends_at < $1`
)

// PGStore is the production SubscriptionStore backed by PostgreSQL
// (subscriptions, migration 000013). A nil pool is a defensive no-op: reads
// return free/nil/empty and writes return errcode.ErrInternal (dev wires
// MemoryStore).
type PGStore struct{ pool PGPool }

// NewPGStore constructs a PGStore over pool.
func NewPGStore(pool PGPool) *PGStore { return &PGStore{pool: pool} }

// CurrentPlan returns the tenant's active plan; no row → free. A nil pool is
// treated as "no subscription configured" → free.
func (s *PGStore) CurrentPlan(ctx context.Context, tenantID int64) (string, error) {
	if s == nil || s.pool == nil {
		return PlanFree, nil
	}
	var plan string
	err := s.pool.QueryRow(ctx, sqlSubCurrentPlan, tenantID).Scan(&plan)
	if errors.Is(err, pgx.ErrNoRows) {
		return PlanFree, nil
	}
	if err != nil {
		return PlanFree, fmt.Errorf("subscription: pg current plan: %w", err)
	}
	return plan, nil
}

// Get returns the tenant's active subscription, or nil if none.
func (s *PGStore) Get(ctx context.Context, tenantID int64) (*Subscription, error) {
	if s == nil || s.pool == nil {
		return nil, nil
	}
	var sub Subscription
	if err := scanSubscription(s.pool.QueryRow(ctx, sqlSubGet, tenantID), &sub); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("subscription: pg get: %w", err)
	}
	return &sub, nil
}

// Upsert cancels prior active rows then inserts a new active row, returning
// the persisted record (id/defaults backfilled from RETURNING). A nil pool
// returns errcode.ErrInternal (writes must not silently drop).
func (s *PGStore) Upsert(ctx context.Context, sub *Subscription) (*Subscription, error) {
	if sub == nil {
		return nil, errcode.ErrParam
	}
	if s == nil || s.pool == nil {
		return nil, errcode.ErrInternal
	}
	status := sub.Status
	if status == "" {
		status = "active"
	}
	plan := sub.Plan
	if plan == "" {
		plan = PlanFree
	}
	if _, err := s.pool.Exec(ctx, sqlSubCancelActive, sub.TenantID); err != nil {
		return nil, fmt.Errorf("subscription: pg cancel active: %w", err)
	}
	var out Subscription
	if err := scanSubscription(
		s.pool.QueryRow(ctx, sqlSubInsert,
			sub.TenantID, plan, sub.PeriodStartedAt, sub.PeriodEndsAt,
			status, sub.Source, sub.PaymentRef, sub.CreatedAt, sub.UpdatedAt,
		), &out,
	); err != nil {
		return nil, fmt.Errorf("subscription: pg insert: %w", err)
	}
	return &out, nil
}

// ListHistory returns every subscription record for the tenant in id order.
// A nil pool (or one without Query) returns an empty slice.
func (s *PGStore) ListHistory(ctx context.Context, tenantID int64) ([]*Subscription, error) {
	out := make([]*Subscription, 0)
	if s == nil || s.pool == nil {
		return out, nil
	}
	q, ok := s.pool.(pgQuerier)
	if !ok {
		return out, nil
	}
	rows, err := q.Query(ctx, sqlSubListHistory, tenantID)
	if err != nil {
		return nil, fmt.Errorf("subscription: pg list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sub Subscription
		if err := scanSubscription(rows, &sub); err != nil {
			return nil, fmt.Errorf("subscription: pg list scan: %w", err)
		}
		out = append(out, &sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("subscription: pg list rows: %w", err)
	}
	return out, nil
}

// ExpireDue transitions every active subscription past its period end to
// expired (CONTRACT §15.8), returning the affected row count. A nil pool is a
// defensive no-op returning 0 (dev wires MemoryStore).
func (s *PGStore) ExpireDue(ctx context.Context, now time.Time) (int, error) {
	if s == nil || s.pool == nil {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, sqlSubExpireDue, now)
	if err != nil {
		return 0, fmt.Errorf("subscription: pg expire due: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// scanner is the common Scan surface of pgx.Row and pgx.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// scanSubscription scans a full subscriptions row into sub, decoding the
// nullable period_ends_at / payment_ref via pgtype (pgxmock-compatible,
// correct for real pgx NULLs).
func scanSubscription(s scanner, sub *Subscription) error {
	var end pgtype.Timestamptz
	var ref pgtype.Text
	if err := s.Scan(
		&sub.ID, &sub.TenantID, &sub.Plan, &sub.PeriodStartedAt, &end,
		&sub.Status, &sub.Source, &ref, &sub.CreatedAt, &sub.UpdatedAt,
	); err != nil {
		return err
	}
	if end.Valid {
		t := end.Time
		sub.PeriodEndsAt = &t
	} else {
		sub.PeriodEndsAt = nil
	}
	if ref.Valid {
		sub.PaymentRef = ref.String
	} else {
		sub.PaymentRef = ""
	}
	return nil
}
