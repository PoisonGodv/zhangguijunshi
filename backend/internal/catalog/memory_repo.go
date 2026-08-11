package catalog

// MemoryProductRepo is an in-memory, concurrency-safe ProductRepo for dev and
// tests. It mirrors the production uniqueness contract (ch05):
//   - (tenant_id, store_id, barcode) is unique when barcode is non-empty;
//   - (tenant_id, store_id, name, unit) is unique.
// Ids are allocated monotonically per process, all reads return defensive copies
// so callers can never mutate the stored record, and every lookup enforces tenant
// isolation (a product owned by another tenant is ErrNotFound).

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultListLimit is used when ProductFilter.Limit <= 0.
const defaultListLimit = 50

type tenantStoreBarcode struct {
	tenant, store int64
	barcode       string
}

type tenantStoreNameUnit struct {
	tenant, store int64
	name, unit    string
}

// MemoryProductRepo stores products keyed by id plus uniqueness indexes.
type MemoryProductRepo struct {
	mu         sync.RWMutex
	byID       map[int64]*Product
	byBarcode  map[tenantStoreBarcode]int64
	byNameUnit map[tenantStoreNameUnit]int64
	nextID     atomic.Int64
}

// NewMemoryProductRepo constructs an empty repo.
func NewMemoryProductRepo() *MemoryProductRepo {
	r := &MemoryProductRepo{
		byID:       make(map[int64]*Product),
		byBarcode:  make(map[tenantStoreBarcode]int64),
		byNameUnit: make(map[tenantStoreNameUnit]int64),
	}
	r.nextID.Store(1000) // product ids start above 1000 to look realistic in logs
	return r
}

// clone returns a defensive deep copy of p (Tags slice is copied too).
func clone(p *Product) *Product {
	if p == nil {
		return nil
	}
	cp := *p
	if p.Tags != nil {
		cp.Tags = append([]string(nil), p.Tags...)
	}
	return &cp
}

// barcodeIndex builds the uniqueness key for a non-empty barcode.
func barcodeIndex(tenantID, storeID int64, barcode string) tenantStoreBarcode {
	return tenantStoreBarcode{tenant: tenantID, store: storeID, barcode: barcode}
}

// nameUnitIndex builds the uniqueness key for (name, unit).
func nameUnitIndex(tenantID, storeID int64, name, unit string) tenantStoreNameUnit {
	return tenantStoreNameUnit{tenant: tenantID, store: storeID, name: name, unit: unit}
}

// Create implements ProductRepo.
func (r *MemoryProductRepo) Create(_ context.Context, p *Product) (*Product, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()

	// Enforce uniqueness before allocating an id.
	if p.Barcode != "" {
		if _, dup := r.byBarcode[barcodeIndex(p.TenantID, p.StoreID, p.Barcode)]; dup {
			return nil, ErrConflict
		}
	}
	if _, dup := r.byNameUnit[nameUnitIndex(p.TenantID, p.StoreID, p.Name, p.Unit)]; dup {
		return nil, ErrConflict
	}

	c := clone(p)
	c.ID = r.nextID.Add(1)
	c.CreatedAt = now
	c.UpdatedAt = now

	r.byID[c.ID] = c
	if c.Barcode != "" {
		r.byBarcode[barcodeIndex(c.TenantID, c.StoreID, c.Barcode)] = c.ID
	}
	r.byNameUnit[nameUnitIndex(c.TenantID, c.StoreID, c.Name, c.Unit)] = c.ID
	return clone(c), nil
}

// Update implements ProductRepo. It replaces the stored record addressed by
// p.ID within p.TenantID, refreshing the uniqueness indexes.
func (r *MemoryProductRepo) Update(_ context.Context, p *Product) (*Product, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.byID[p.ID]
	if !ok || old.TenantID != p.TenantID {
		return nil, ErrNotFound
	}

	// Re-check uniqueness, ignoring the record's own current keys.
	if p.Barcode != "" {
		if id, dup := r.byBarcode[barcodeIndex(p.TenantID, p.StoreID, p.Barcode)]; dup && id != p.ID {
			return nil, ErrConflict
		}
	}
	if id, dup := r.byNameUnit[nameUnitIndex(p.TenantID, p.StoreID, p.Name, p.Unit)]; dup && id != p.ID {
		return nil, ErrConflict
	}

	// Drop stale indexes keyed by the old values.
	if old.Barcode != "" {
		delete(r.byBarcode, barcodeIndex(old.TenantID, old.StoreID, old.Barcode))
	}
	delete(r.byNameUnit, nameUnitIndex(old.TenantID, old.StoreID, old.Name, old.Unit))

	c := clone(p)
	c.CreatedAt = old.CreatedAt // creation stamp is immutable
	c.UpdatedAt = time.Now()

	r.byID[c.ID] = c
	if c.Barcode != "" {
		r.byBarcode[barcodeIndex(c.TenantID, c.StoreID, c.Barcode)] = c.ID
	}
	r.byNameUnit[nameUnitIndex(c.TenantID, c.StoreID, c.Name, c.Unit)] = c.ID
	return clone(c), nil
}

// GetByID implements ProductRepo.
func (r *MemoryProductRepo) GetByID(_ context.Context, tenantID, id int64) (*Product, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byID[id]
	if !ok || p.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return clone(p), nil
}

// GetByBarcode implements ProductRepo.
func (r *MemoryProductRepo) GetByBarcode(_ context.Context, tenantID, storeID int64, barcode string) (*Product, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	id, ok := r.byBarcode[barcodeIndex(tenantID, storeID, barcode)]
	if !ok {
		return nil, ErrNotFound
	}
	p, ok := r.byID[id]
	if !ok || p.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return clone(p), nil
}

// matches reports whether p satisfies the optional filters (StoreID/Category/
// Query) within tenantID. Shared by List and ListPage so both filter
// identically.
func matchProduct(p *Product, tenantID int64, f ProductFilter) bool {
	if p.TenantID != tenantID {
		return false
	}
	if f.StoreID > 0 && p.StoreID != f.StoreID {
		return false
	}
	if f.Category != "" && p.Category != f.Category {
		return false
	}
	if f.Query != "" && !strings.Contains(p.Name, f.Query) && !strings.Contains(p.Barcode, f.Query) {
		return false
	}
	return true
}

// eachInIDOrder iterates stored products in ascending-id (insertion) order for
// deterministic pagination, invoking fn until it returns false.
func (r *MemoryProductRepo) eachInIDOrder(fn func(*Product) bool) {
	for id := int64(1001); ; id++ { // ids allocated from 1001 upward
		p, ok := r.byID[id]
		if !ok {
			if id > r.nextID.Load() {
				return
			}
			continue
		}
		if !fn(p) {
			return
		}
	}
}

// List implements ProductRepo. Filters are applied within tenantID; results are
// returned in insertion (id) order for deterministic pagination.
func (r *MemoryProductRepo) List(_ context.Context, tenantID int64, f ProductFilter) ([]*Product, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	matched := make([]*Product, 0, limit)
	seen := 0
	r.eachInIDOrder(func(p *Product) bool {
		if !matchProduct(p, tenantID, f) {
			return true
		}
		if seen < offset {
			seen++
			return true
		}
		matched = append(matched, clone(p))
		return len(matched) < limit
	})
	return matched, nil
}

// ListPage implements the optional ProductLister capability (CONTRACT §24.5):
// one pass filters+counts every matching row (Total), a second slices out the
// offset/limit window (Items) — so Total is the FULL match count, not len(Items).
func (r *MemoryProductRepo) ListPage(_ context.Context, tenantID int64, f ProductFilter) (ListProductsResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	limit := f.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var total int64
	r.eachInIDOrder(func(p *Product) bool {
		if matchProduct(p, tenantID, f) {
			total++
		}
		return true
	})

	items := make([]*Product, 0, limit)
	seen := int64(0)
	r.eachInIDOrder(func(p *Product) bool {
		if !matchProduct(p, tenantID, f) {
			return true
		}
		if seen < int64(offset) {
			seen++
			return true
		}
		items = append(items, clone(p))
		return len(items) < limit
	})
	return ListProductsResult{Items: items, Total: total}, nil
}

// UpdateTags implements ProductRepo. It replaces ONLY the tag set of the product
// addressed by productID within tenantID (BE-A-09 write-back), leaving every
// other field — and the uniqueness indexes — untouched. ErrNotFound if the
// product is absent or owned by another tenant. The stored tags are a defensive
// copy so the caller can never mutate them through the passed slice.
func (r *MemoryProductRepo) UpdateTags(_ context.Context, tenantID, productID int64, tags []string) (*Product, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.byID[productID]
	if !ok || old.TenantID != tenantID {
		return nil, ErrNotFound
	}

	c := clone(old)
	c.Tags = append([]string(nil), tags...)
	c.UpdatedAt = time.Now()
	r.byID[productID] = c
	return clone(c), nil
}
