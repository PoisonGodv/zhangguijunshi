package inventory

// MemoryBatchRepo is an in-memory, concurrency-safe BatchRepo for dev and
// tests. It enforces the same tenant-isolation contract as the production pgx
// repo: a read or deduction for tenant A never sees tenant B's batches or
// movements (cross-tenant access is indistinguishable from absence). All reads
// return defensive copies (including the ExpiryDate/ExpiryWarnDate pointers) so
// callers cannot mutate stored records.
//
// Layout: batches live in a byID map (each tagged with its tenant), and a
// per-(tenant,store,product) ordered slice of batch ids preserves insertion
// order so ListActiveBatchesFIFO is O(lots) and always returns oldest-first.
// Movements live in a separate byID map plus an insertion-ordered id list.

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// defaultMovementLimit is used when MovementFilter.Limit <= 0.
const defaultMovementLimit = 50

// defaultBatchListLimit is used when BatchFilter.Limit <= 0 (BE-A, §24.2).
const defaultBatchListLimit = 50

// nowFunc is the repo clock. It is a package-level variable so tests can pin it
// for deterministic CreatedAt/UpdatedAt; production leaves it as time.Now. All
// access goes through now() under a mutex so -race stays clean.
var (
	nowMu   sync.RWMutex
	nowFunc = time.Now
)

func now() time.Time {
	nowMu.RLock()
	defer nowMu.RUnlock()
	return nowFunc()
}

// setNowForTest swaps the repo clock and returns a restore func. Test-only.
func setNowForTest(f func() time.Time) func() {
	nowMu.Lock()
	prev := nowFunc
	nowFunc = f
	nowMu.Unlock()
	return func() {
		nowMu.Lock()
		nowFunc = prev
		nowMu.Unlock()
	}
}

// stockKey identifies one product's stock within one store of one tenant.
type stockKey struct {
	tenant, store, product int64
}

// MemoryBatchRepo stores batches and movements in memory.
type MemoryBatchRepo struct {
	mu sync.RWMutex

	batches   map[int64]*Batch
	stockFIFO map[stockKey][]int64 // batch ids in insertion order per stockKey
	batchSeq  atomic.Int64

	movements   map[int64]*Movement
	movementIDs []int64 // insertion order
	movementSeq atomic.Int64
}

// NewMemoryBatchRepo constructs an empty repo.
func NewMemoryBatchRepo() *MemoryBatchRepo {
	r := &MemoryBatchRepo{
		batches:   make(map[int64]*Batch),
		stockFIFO: make(map[stockKey][]int64),
		movements: make(map[int64]*Movement),
	}
	r.batchSeq.Store(7000)    // batch ids start above 7000 to look realistic in logs
	r.movementSeq.Store(9000) // movement ids live in their own range
	return r
}

// CreateBatch implements BatchRepo. The batch is deep-copied so later caller
// mutation of the input never leaks into the store.
func (r *MemoryBatchRepo) CreateBatch(_ context.Context, b *Batch) (*Batch, error) {
	ts := now()
	r.mu.Lock()
	defer r.mu.Unlock()

	stored := cloneBatch(b)
	stored.ID = r.batchSeq.Add(1)
	stored.CreatedAt = ts
	stored.UpdatedAt = ts

	r.batches[stored.ID] = stored
	k := stockKey{tenant: stored.TenantID, store: stored.StoreID, product: stored.ProductID}
	r.stockFIFO[k] = append(r.stockFIFO[k], stored.ID)
	return cloneBatch(stored), nil
}

// GetBatch implements BatchRepo. Cross-tenant ids return ErrNotFound.
func (r *MemoryBatchRepo) GetBatch(_ context.Context, tenantID, id int64) (*Batch, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.batches[id]
	if !ok || b.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneBatch(b), nil
}

// UpdateBatch implements BatchRepo. It replaces the stored record addressed by
// b.ID within b.TenantID; the stock FIFO order is unaffected (deduction does
// not change creation order). A cleared lot stays in the FIFO list but is
// filtered out by ListActiveBatchesFIFO because its quantity is 0 / status
// differs from active.
func (r *MemoryBatchRepo) UpdateBatch(_ context.Context, b *Batch) (*Batch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.batches[b.ID]
	if !ok || old.TenantID != b.TenantID {
		return nil, ErrNotFound
	}

	stored := cloneBatch(b)
	stored.CreatedAt = old.CreatedAt // creation stamp is immutable
	stored.UpdatedAt = now()
	r.batches[stored.ID] = stored
	return cloneBatch(stored), nil
}

// ListActiveBatchesFIFO implements BatchRepo. It walks the stockKey's
// insertion-ordered id list and returns only lots that are still active with a
// positive quantity, oldest first. Results are defensive copies.
func (r *MemoryBatchRepo) ListActiveBatchesFIFO(_ context.Context, tenantID, storeID, productID int64) ([]*Batch, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	k := stockKey{tenant: tenantID, store: storeID, product: productID}
	ids := r.stockFIFO[k]
	out := make([]*Batch, 0, len(ids))
	for _, id := range ids {
		b, ok := r.batches[id]
		if !ok {
			continue
		}
		// Defence-in-depth: the index is already tenant-scoped, but never trust it.
		if b.TenantID != tenantID {
			continue
		}
		if b.Status != BatchStatusActive || b.Quantity <= 0 {
			continue
		}
		out = append(out, cloneBatch(b))
	}
	// Insertion order already equals FIFO order (ids are monotonic with
	// CreatedAt), but sort defensively so the contract holds even if a batch was
	// back-dated by a future caller.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ListNearingBatches implements BatchRepo (M3-S2). It scans every batch in the
// repo under a read lock and returns the active, positive-quantity lots of
// (tenantID, storeID) whose ExpiryDate is set and falls on or before
// now+withinDays. Cross-tenant / cross-store lots are invisible. Results are
// defensive copies ordered by batch id (deterministic for tests).
func (r *MemoryBatchRepo) ListNearingBatches(_ context.Context, tenantID, storeID int64, withinDays int) ([]*Batch, error) {
	deadline := now().AddDate(0, 0, withinDays)

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Collect matching ids in ascending order so output is deterministic.
	ids := make([]int64, 0)
	for id, b := range r.batches {
		if b.TenantID != tenantID || b.StoreID != storeID {
			continue
		}
		if b.Status != BatchStatusActive || b.Quantity <= 0 {
			continue
		}
		if b.ExpiryDate == nil || b.ExpiryDate.After(deadline) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]*Batch, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneBatch(r.batches[id]))
	}
	return out, nil
}

// EarliestExpiryByProduct implements the optional ProductExpirer capability
// (M7-A). It scans every batch under a read lock and, for tenantID, returns each
// product's earliest (soonest) ExpiryDate across its active, positive-quantity
// lots that carry a non-nil ExpiryDate. storeID=0 spans all of the tenant's
// stores; storeID>0 scopes to that one store. Cross-tenant lots are invisible.
// The returned times are fresh copies so callers cannot alias stored state.
func (r *MemoryBatchRepo) EarliestExpiryByProduct(_ context.Context, tenantID, storeID int64) (map[int64]time.Time, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[int64]time.Time)
	for _, b := range r.batches {
		if b.TenantID != tenantID {
			continue
		}
		if storeID > 0 && b.StoreID != storeID {
			continue
		}
		if b.Status != BatchStatusActive || b.Quantity <= 0 {
			continue
		}
		if b.ExpiryDate == nil {
			continue
		}
		cur, ok := out[b.ProductID]
		if !ok || b.ExpiryDate.Before(cur) {
			out[b.ProductID] = *b.ExpiryDate // fresh value copy, no aliasing
		}
	}
	return out, nil
}

// ListBatches implements the optional BatchLister capability (BE-A, §24.2). It
// scans every batch in the repo under a read lock and returns the ctx tenant's
// lots matching f (store/product/status/expiry_before), ordered by batch id for
// deterministic pagination, with Limit/Offset applied. Unlike
// ListActiveBatchesFIFO / ListNearingBatches it returns EVERY status
// (active/nearing/expired/cleared) so the boss 临期看板 sees cleared/expired lots
// too — it only READS status. Cross-tenant lots are invisible. Results are
// defensive copies.
func (r *MemoryBatchRepo) ListBatches(_ context.Context, tenantID int64, f BatchFilter) ([]*Batch, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	limit := f.Limit
	if limit <= 0 {
		limit = defaultBatchListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	// Collect matching ids in ascending order so output is deterministic.
	ids := make([]int64, 0)
	for id, b := range r.batches {
		if matchBatch(b, tenantID, f) {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]*Batch, 0, limit)
	for _, id := range ids {
		if offset > 0 {
			offset--
			continue
		}
		out = append(out, cloneBatch(r.batches[id]))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// ListBatchesCount implements the optional BatchLister capability (BE-A, §24.2).
// It returns the total count of the ctx tenant's batches matching f (same
// predicate as ListBatches) WITHOUT Limit/Offset, so the pager can render the
// full total. Cross-tenant lots are invisible.
func (r *MemoryBatchRepo) ListBatchesCount(_ context.Context, tenantID int64, f BatchFilter) (int64, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var n int64
	for _, b := range r.batches {
		if matchBatch(b, tenantID, f) {
			n++
		}
	}
	return n, nil
}

// matchBatch reports whether b belongs to tenantID and matches the non-paging
// fields of f (StoreID/ProductID/Status/ExpiryBefore). Limit/Offset are ignored
// so the same predicate drives both the page slice and the total count.
func matchBatch(b *Batch, tenantID int64, f BatchFilter) bool {
	if b.TenantID != tenantID {
		return false
	}
	if f.StoreID > 0 && b.StoreID != f.StoreID {
		return false
	}
	if f.ProductID > 0 && b.ProductID != f.ProductID {
		return false
	}
	if f.Status != "" && b.Status != f.Status {
		return false
	}
	if f.ExpiryBefore != nil {
		// A lot with no expiry never satisfies a "expiry before cutoff" filter.
		if b.ExpiryDate == nil || b.ExpiryDate.After(*f.ExpiryBefore) {
			return false
		}
	}
	return true
}

// AppendMovement implements BatchRepo. The movement is deep-copied and assigned
// an id; CreatedAt is stamped by the caller (service) so it can share the
// injected clock.
func (r *MemoryBatchRepo) AppendMovement(_ context.Context, m *Movement) (*Movement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored := cloneMovement(m)
	stored.ID = r.movementSeq.Add(1)
	r.movements[stored.ID] = stored
	r.movementIDs = append(r.movementIDs, stored.ID)
	return cloneMovement(stored), nil
}

// ConsumeFIFOAtomic implements the optional StockConsumer capability. The whole
// read-plan-apply-movement cycle runs under a single write lock, so concurrent
// consumers serialise and stock can never oversell. mkMovement is invoked once
// per touched batch (inside the lock) so the service stamps time/operator on
// each ledger entry.
func (r *MemoryBatchRepo) ConsumeFIFOAtomic(_ context.Context, tenantID, storeID, productID, qty int64, mkMovement func(batchID, deducted int64) *Movement) ([]*Batch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	k := stockKey{tenant: tenantID, store: storeID, product: productID}
	ids := r.stockFIFO[k]

	// Gather the active, non-empty lots oldest-first (same predicate as
	// ListActiveBatchesFIFO, but here we keep references to the STORED records
	// because we are about to mutate them atomically under the lock).
	active := make([]*Batch, 0, len(ids))
	for _, id := range ids {
		b, ok := r.batches[id]
		if !ok || b.TenantID != tenantID {
			continue
		}
		if b.Status != BatchStatusActive || b.Quantity <= 0 {
			continue
		}
		active = append(active, b)
	}

	// Dry-run the plan against stored records and verify full coverage.
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
		return nil, ErrInsufficientStock
	}

	// Coverage confirmed: mutate stored batches and append movements atomically.
	ts := now()
	updated := make([]*Batch, 0, len(plans))
	for _, p := range plans {
		p.batch.Quantity -= p.deduct
		if p.batch.Quantity == 0 {
			p.batch.Status = BatchStatusCleared
		}
		p.batch.UpdatedAt = ts

		mv := mkMovement(p.batch.ID, p.deduct)
		if mv != nil {
			stored := cloneMovement(mv)
			stored.ID = r.movementSeq.Add(1)
			// Stamp tenant/store/product defensively from the batch so a ledger
			// line can never be written under a different tenant than the lot.
			stored.TenantID = tenantID
			stored.StoreID = storeID
			stored.ProductID = productID
			stored.Type = MovementTypeSaleOut
			r.movements[stored.ID] = stored
			r.movementIDs = append(r.movementIDs, stored.ID)
		}
		updated = append(updated, cloneBatch(p.batch))
	}
	return updated, nil
}

// ListMovements implements BatchRepo. Filters apply within tenantID only; Day
// filters by the natural calendar day (in Day's own location). Results are in
// insertion (id) order for deterministic pagination.
func (r *MemoryBatchRepo) ListMovements(_ context.Context, tenantID int64, f MovementFilter) ([]*Movement, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	limit := f.Limit
	if limit <= 0 {
		limit = defaultMovementLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var dy, dd int
	var dmo time.Month
	var dloc *time.Location
	filterDay := f.Day != nil
	if filterDay {
		dy, dmo, dd = f.Day.Date()
		dloc = f.Day.Location()
	}

	out := make([]*Movement, 0, limit)
	seen := 0
	for _, id := range r.movementIDs {
		m, ok := r.movements[id]
		if !ok {
			continue
		}
		if m.TenantID != tenantID {
			continue
		}
		if f.StoreID > 0 && m.StoreID != f.StoreID {
			continue
		}
		if f.ProductID > 0 && m.ProductID != f.ProductID {
			continue
		}
		if f.Type != "" && m.Type != f.Type {
			continue
		}
		if filterDay {
			cy, cm, cd := m.CreatedAt.In(dloc).Date()
			if cy != dy || cm != dmo || cd != dd {
				continue
			}
		}
		if seen < offset {
			seen++
			continue
		}
		out = append(out, cloneMovement(m))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// cloneBatch returns a deep copy of b (date pointers re-pointed at fresh time
// values) so internal state is never aliased.
func cloneBatch(b *Batch) *Batch {
	if b == nil {
		return nil
	}
	cp := *b
	if b.ExpiryDate != nil {
		t := *b.ExpiryDate
		cp.ExpiryDate = &t
	}
	if b.ExpiryWarnDate != nil {
		t := *b.ExpiryWarnDate
		cp.ExpiryWarnDate = &t
	}
	return &cp
}

// cloneMovement returns a copy of m (no pointer fields, but kept symmetric with
// cloneBatch for clarity).
func cloneMovement(m *Movement) *Movement {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}
