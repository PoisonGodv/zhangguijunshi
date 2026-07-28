package strategy

// MemoryStrategyRepo is the in-memory, concurrency-safe StrategyRepo for dev
// and tests (CONTRACT §12.3.1). It enforces the same tenant-isolation contract
// as the production pgx repo: a read or mutation scoped to tenant A never sees
// tenant B's strategies / items / feedback — cross-tenant access returns
// ErrNotFound, indistinguishable from absence. All reads return defensive deep
// copies so callers cannot mutate stored records.
//
// Storage layout (single source of truth, no drift between strategy and items):
//   - strategies: the daily advice rows WITHOUT items attached (Items == nil).
//   - items:      every StrategyItem by id; Get/GetToday/List re-attach a
//                 strategy's items by scanning this map and sorting by Rank, so
//                 UpdateItemStatus (mutating an item) is always reflected.
//   - feedback:   keyed by strategyItemID (1:1, mirroring UNIQUE(tenant,item)).
//   - daily:      (tenant, store, forDate) -> strategyID, the upsert key.
//
// SaveAdvice (rewrite row + rebuild items) and UpdateItemStatus (state-machine
// transition) are the two critical sections, each serialised under the repo's
// single write lock so the in-memory flow has the atomicity the pgx
// implementation will get from a real transaction.

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// dailyKey is the upsert key for SaveAdvice (CONTRACT §12.5.8).
type dailyKey struct {
	tenantID int64
	storeID  int64
	forDate  string
}

// MemoryStrategyRepo stores strategies, items and feedback.
type MemoryStrategyRepo struct {
	mu          sync.RWMutex
	strategies  map[int64]*Strategy         // by strategy id (Items nil; items live in `items`)
	items       map[int64]*StrategyItem     // by item id
	feedback    map[int64]*StrategyFeedback // by strategyItemID (UNIQUE per item)
	daily       map[dailyKey]int64          // (tenant,store,date) -> strategyID
	strategySeq int64
	itemSeq     int64
	feedbackSeq int64
}

// NewMemoryStrategyRepo constructs an empty MemoryStrategyRepo.
func NewMemoryStrategyRepo() *MemoryStrategyRepo {
	return &MemoryStrategyRepo{
		strategies: make(map[int64]*Strategy),
		items:      make(map[int64]*StrategyItem),
		feedback:   make(map[int64]*StrategyFeedback),
		daily:      make(map[dailyKey]int64),
	}
}

// itemsOfLocked returns deep-copied items belonging to (tenantID, strategyID),
// sorted by Rank ascending. Caller must hold r.mu (R or W).
func (r *MemoryStrategyRepo) itemsOfLocked(tenantID, strategyID int64) []StrategyItem {
	var out []StrategyItem
	for _, it := range r.items {
		if it.StrategyID == strategyID && it.TenantID == tenantID {
			out = append(out, cloneItem(*it))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rank < out[j].Rank })
	return out
}

// SaveAdvice atomically upserts the advice for (tenant, store, forDate).
//
// On first write a new strategy id is allocated and CreatedAt stamped. On
// same-day regeneration the existing row is overwritten in place (preserving
// id + CreatedAt), its old items are discarded along with their feedback, and
// fresh items are allocated. Strategy.ID and Items[].ID are backfilled on the
// input pointer; a deep clone is returned.
func (r *MemoryStrategyRepo) SaveAdvice(_ context.Context, s *Strategy) (*Strategy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := dailyKey{tenantID: s.TenantID, storeID: s.StoreID, forDate: s.ForDate}
	now := time.Now()

	var id int64
	var createdAt time.Time
	if existingID, ok := r.daily[key]; ok {
		id = existingID
		if prev := r.strategies[id]; prev != nil {
			createdAt = prev.CreatedAt
		}
		// Rebuild items: drop old items + their feedback.
		for itemID, it := range r.items {
			if it.StrategyID == id {
				delete(r.items, itemID)
				delete(r.feedback, itemID)
			}
		}
	} else {
		id = atomic.AddInt64(&r.strategySeq, 1)
	}
	if createdAt.IsZero() {
		createdAt = now
	}

	s.ID = id
	s.CreatedAt = createdAt
	// Backfill item ids + ownership, then store fresh clones.
	for i := range s.Items {
		s.Items[i].ID = atomic.AddInt64(&r.itemSeq, 1)
		s.Items[i].StrategyID = id
		s.Items[i].TenantID = s.TenantID
		if s.Items[i].ExecutionStatus == "" {
			s.Items[i].ExecutionStatus = ItemPending
		}
		ic := cloneItem(s.Items[i])
		r.items[s.Items[i].ID] = &ic
	}

	// Store the strategy row without items (items live in r.items).
	stored := CloneStrategy(s)
	stored.Items = nil
	r.strategies[id] = stored
	r.daily[key] = id

	return CloneStrategy(s), nil
}

// Get loads one strategy (with items) by id within tenantID. ErrNotFound when
// absent or owned by another tenant.
func (r *MemoryStrategyRepo) Get(_ context.Context, tenantID, id int64) (*Strategy, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st, ok := r.strategies[id]
	if !ok || st.TenantID != tenantID {
		return nil, ErrNotFound
	}
	cp := CloneStrategy(st)
	cp.Items = r.itemsOfLocked(tenantID, id)
	return cp, nil
}

// GetToday loads the advice for (tenant, store, date). ErrNotFound/跨租户.
func (r *MemoryStrategyRepo) GetToday(_ context.Context, tenantID, storeID int64, date string) (*Strategy, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.daily[dailyKey{tenantID: tenantID, storeID: storeID, forDate: date}]
	if !ok {
		return nil, ErrNotFound
	}
	st, ok := r.strategies[id]
	if !ok || st.TenantID != tenantID {
		return nil, ErrNotFound
	}
	cp := CloneStrategy(st)
	cp.Items = r.itemsOfLocked(tenantID, id)
	return cp, nil
}

// List returns strategies matching the filter for the ctx tenant, ordered by ID
// descending (newest first). Zero-value filter fields are not applied; Limit<=0
// defaults to 50.
func (r *MemoryStrategyRepo) List(_ context.Context, tenantID int64, f StrategyFilter) ([]*Strategy, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	var out []*Strategy
	for _, st := range r.strategies {
		if st.TenantID != tenantID {
			continue
		}
		if f.StoreID > 0 && st.StoreID != f.StoreID {
			continue
		}
		if f.Date != "" && st.ForDate != f.Date {
			continue
		}
		if f.Status != "" && st.Status != f.Status {
			continue
		}
		cp := CloneStrategy(st)
		cp.Items = r.itemsOfLocked(tenantID, st.ID)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if f.Offset > 0 && f.Offset < len(out) {
		out = out[f.Offset:]
	} else if f.Offset >= len(out) {
		return []*Strategy{}, nil
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetItem loads one item by id within tenantID. ErrNotFound/跨租户.
func (r *MemoryStrategyRepo) GetItem(_ context.Context, tenantID, itemID int64) (*StrategyItem, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	it, ok := r.items[itemID]
	if !ok || it.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return CloneItem(it), nil
}

// UpdateItemStatus applies one execution_status transition under the repo write
// lock and stamps executedAt. Illegal transitions yield ErrParam (checked under
// the lock so the read-modify-write is race-safe); absent/cross-tenant items
// yield ErrNotFound.
func (r *MemoryStrategyRepo) UpdateItemStatus(_ context.Context, tenantID, itemID int64, status string, executedAt *time.Time, _ time.Time) (*StrategyItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	it, ok := r.items[itemID]
	if !ok || it.TenantID != tenantID {
		return nil, ErrNotFound
	}
	if !isAllowedItemTransition(it.ExecutionStatus, status) {
		return nil, ErrParam
	}
	it.ExecutionStatus = status
	it.ExecutedAt = executedAt
	return CloneItem(it), nil
}

// RecordFeedback upserts the boss's feedback for one item. UNIQUE(tenant, item)
// → a repeat overwrites Useful/CreatedAt, keeping the feedback ID. Absent or
// cross-tenant items yield ErrNotFound.
func (r *MemoryStrategyRepo) RecordFeedback(_ context.Context, tenantID, strategyItemID int64, useful bool, now time.Time) (*StrategyFeedback, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	it, ok := r.items[strategyItemID]
	if !ok || it.TenantID != tenantID {
		return nil, ErrNotFound
	}
	if existing, ok := r.feedback[strategyItemID]; ok {
		existing.Useful = useful
		existing.CreatedAt = now
		return cloneFeedback(existing), nil
	}
	fb := &StrategyFeedback{
		ID:             atomic.AddInt64(&r.feedbackSeq, 1),
		TenantID:       tenantID,
		StrategyItemID: strategyItemID,
		Useful:         useful,
		CreatedAt:      now,
	}
	r.feedback[strategyItemID] = fb
	return cloneFeedback(fb), nil
}
