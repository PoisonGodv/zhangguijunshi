package store

// MemoryStoreRepo is an in-memory, concurrency-safe StoreRepo for dev and
// tests. It enforces the same tenant-isolation contract as the production pgx
// repo: a read scoped to tenant A never sees tenant B's stores. Reads return
// defensive copies so callers cannot mutate stored records.
//
// The repo keeps the store table keyed by id plus a tenant index, and seeds
// two demo stores under tenant_id=1 at construction so a fresh dev box has
// something to render on the C 端 门店 list (production populates stores via
// tenant onboarding; the seed is dev-only convenience).
//
// The StoreID range starts at 7000 so it reads realistically in logs and never
// collides with other domains' in-memory id ranges.

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryStoreRepo stores stores in maps.
type MemoryStoreRepo struct {
	mu sync.RWMutex

	storesByID  map[int64]*Store
	storesByTen map[int64][]int64 // tenantID -> ordered store ids

	nextID atomic.Int64
}

// NewMemoryStoreRepo constructs an empty repo and seeds two demo stores under
// tenant_id=1 (the dev / test convenience seed). A fresh tenant with no seeded
// stores yields an empty list (ARCH-01: cross-tenant is empty, never a leak).
func NewMemoryStoreRepo() *MemoryStoreRepo {
	r := &MemoryStoreRepo{
		storesByID:  make(map[int64]*Store),
		storesByTen: make(map[int64][]int64),
	}
	r.nextID.Store(7000)
	r.seed()
	return r
}

// seed inserts two demo stores under tenant_id=1 (the dev / test convenience
// seed so the C 端 门店 list renders something on a fresh boot). Production
// populates stores via tenant onboarding; this seed is dev-only.
func (r *MemoryStoreRepo) seed() {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.createInternal(&Store{
		TenantID: 1, Name: "总店 · 阳光水果行", Address: "上海市浦东新区阳光路1号",
		Phone: "021-50000001", GroupQR: "group_qr_seed_1", Active: true, CreatedAt: now,
	})
	r.createInternal(&Store{
		TenantID: 1, Name: "分店 · 阳光水果行(临港)", Address: "上海市浦东新区临港新片区芦潮港路2号",
		Phone: "021-50000002", GroupQR: "group_qr_seed_2", Active: true, CreatedAt: now,
	})
}

// createInternal stores s with a fresh id and returns the stored record (with
// its assigned ID); caller MUST hold r.mu. The returned pointer is the repo's
// own record — callers that hand it out MUST clone it first.
func (r *MemoryStoreRepo) createInternal(s *Store) *Store {
	stored := cloneStore(s)
	stored.ID = r.nextID.Add(1)
	r.storesByID[stored.ID] = stored
	r.storesByTen[stored.TenantID] = append(r.storesByTen[stored.TenantID], stored.ID)
	return stored
}

// Seed inserts a demo store for the given tenant and returns it (with the
// repo-assigned ID). Used by tests to populate a non-default tenant (the
// production-style onboarding path is not yet wired). It is NOT part of the
// StoreRepo interface.
func (r *MemoryStoreRepo) Seed(s *Store) *Store {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneStore(r.createInternal(s))
}

// ListStores implements StoreRepo. It returns the active stores of tenantID,
// ordered by store id (deterministic for tests). Cross-tenant stores are never
// visible; inactive stores are filtered out.
func (r *MemoryStoreRepo) ListStores(_ context.Context, tenantID int64) ([]*Store, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]int64, len(r.storesByTen[tenantID]))
	copy(ids, r.storesByTen[tenantID])
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]*Store, 0, len(ids))
	for _, id := range ids {
		s := r.storesByID[id]
		if s == nil || s.TenantID != tenantID || !s.Active {
			continue
		}
		out = append(out, cloneStore(s))
	}
	return out, nil
}

// cloneStore returns a copy of s so internal state is never aliased.
func cloneStore(s *Store) *Store {
	if s == nil {
		return nil
	}
	cp := *s
	return &cp
}
