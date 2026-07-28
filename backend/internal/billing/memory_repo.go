package billing

// MemoryOrderRepo is an in-memory, concurrency-safe OrderRepo for dev and
// tests. It enforces the same tenant-isolation contract as the production
// pgx repo: a GetByID/ListByDate for tenant A never sees tenant B's orders
// (cross-tenant reads are indistinguishable from absence). All reads return
// defensive copies (including the Items slice) so callers cannot mutate stored
// records.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryOrderRepo stores orders keyed by id, each tagged with its tenant.
type MemoryOrderRepo struct {
	mu     sync.RWMutex
	byID   map[int64]*Order
	nextID atomic.Int64
}

// NewMemoryOrderRepo constructs an empty repo.
func NewMemoryOrderRepo() *MemoryOrderRepo {
	r := &MemoryOrderRepo{byID: make(map[int64]*Order)}
	r.nextID.Store(5000) // order ids start above 5000 to look realistic in logs
	return r
}

// Create implements OrderRepo. It deep-copies the order (including Items) so
// later caller mutation of the input never leaks into the store.
func (r *MemoryOrderRepo) Create(_ context.Context, o *Order) (*Order, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored := cloneOrder(o)
	stored.ID = r.nextID.Add(1)
	r.byID[stored.ID] = stored
	return cloneOrder(stored), nil
}

// GetByID implements OrderRepo. Cross-tenant ids return ErrNotFound.
func (r *MemoryOrderRepo) GetByID(_ context.Context, tenantID, id int64) (*Order, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	o, ok := r.byID[id]
	if !ok || o.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneOrder(o), nil
}

// ListByDate implements OrderRepo. It filters by tenant + optional store and
// by the natural calendar day (in day's own location) containing day.
func (r *MemoryOrderRepo) ListByDate(_ context.Context, tenantID, storeID int64, day time.Time) ([]*Order, error) {
	y, m, d := day.Date()
	loc := day.Location()

	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Order, 0)
	for _, o := range r.byID {
		if o.TenantID != tenantID {
			continue
		}
		if storeID > 0 && o.StoreID != storeID {
			continue
		}
		cy, cm, cd := o.CreatedAt.In(loc).Date()
		if cy == y && cm == m && cd == d {
			out = append(out, cloneOrder(o))
		}
	}
	return out, nil
}

// OrderTotalByMembers implements OrderRepo. It sums the total_cents of the
// tenant's PAID orders matching memberIDs within [since, until] (paid-time
// inclusive). Empty memberIDs or since.After(until) yields 0. Only orders
// owned by tenantID are considered (cross-tenant orders are skipped).
func (r *MemoryOrderRepo) OrderTotalByMembers(_ context.Context, tenantID int64, memberIDs []int64, since, until time.Time) (int64, error) {
	if len(memberIDs) == 0 || since.After(until) {
		return 0, nil
	}
	set := make(map[int64]struct{}, len(memberIDs))
	for _, id := range memberIDs {
		set[id] = struct{}{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var total int64
	for _, o := range r.byID {
		if o.TenantID != tenantID || o.Status != orderStatusPaid {
			continue
		}
		if _, ok := set[o.MemberID]; !ok {
			continue
		}
		if !orderPaidInRange(o, since, until) {
			continue
		}
		total += o.TotalCents
	}
	return total, nil
}

// RepurchaseMembers implements OrderRepo. It counts the distinct memberIDs
// that have at least one PAID order within [since, until] (paid-time
// inclusive) in tenantID. Empty memberIDs or since.After(until) yields 0.
func (r *MemoryOrderRepo) RepurchaseMembers(_ context.Context, tenantID int64, memberIDs []int64, since, until time.Time) (int, error) {
	if len(memberIDs) == 0 || since.After(until) {
		return 0, nil
	}
	set := make(map[int64]struct{}, len(memberIDs))
	for _, id := range memberIDs {
		set[id] = struct{}{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	repurchased := make(map[int64]struct{})
	for _, o := range r.byID {
		if o.TenantID != tenantID || o.Status != orderStatusPaid {
			continue
		}
		if _, ok := set[o.MemberID]; !ok {
			continue
		}
		if !orderPaidInRange(o, since, until) {
			continue
		}
		repurchased[o.MemberID] = struct{}{}
	}
	return len(repurchased), nil
}

// orderPaidInRange reports whether o was paid within [since, until]
// (inclusive). Orders without a PaidAt are not in range (paid orders always
// carry PaidAt, set by CreateOrder; non-paid statuses are filtered upstream).
func orderPaidInRange(o *Order, since, until time.Time) bool {
	if o.PaidAt == nil {
		return false
	}
	t := *o.PaidAt
	if t.Before(since) || t.After(until) {
		return false
	}
	return true
}

// cloneOrder returns a deep copy of o (Items slice copied, PaidAt pointer
// re-pointed at a fresh time value) so internal state is never aliased.
func cloneOrder(o *Order) *Order {
	if o == nil {
		return nil
	}
	cp := *o
	if o.Items != nil {
		cp.Items = make([]OrderItem, len(o.Items))
		copy(cp.Items, o.Items)
	}
	if o.PaidAt != nil {
		t := *o.PaidAt
		cp.PaidAt = &t
	}
	return &cp
}
