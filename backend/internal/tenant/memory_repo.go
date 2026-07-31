package tenant

// memory_repo.go is an in-memory, concurrency-safe Repo for dev and tests. It
// models the cross-tenant read surface directly: it holds every tenant plus a
// small set of per-tenant counts (product / member / order / store), so the
// platform aggregate view renders without importing catalog or membership
// (anti-corruption-clean, like internal/store).
//
// Because this repo is cross-tenant BY DESIGN, it enforces the platform gate
// itself (ensurePlatform): a ctx without a platform/system Source yields an
// empty page rather than a leak, so even a caller that bypasses the Service
// cannot read across tenants (ARCH-01). Reads return defensive copies.
//
// Construction seeds two demo tenants (id 1 and 2) with non-zero counts so a
// fresh dev box has something to render on the platform 租户 list; production
// reads the real tenants table via the PG repo. The tenant id range starts at
// the BIGSERIAL values the seed uses so it reads realistically in logs.

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// memoryRecord is one stored tenant plus its per-tenant counts. The counts are
// stored alongside the identity so the memory repo can answer the aggregate
// view without joining other domains.
type memoryRecord struct {
	t    Tenant
	prod int
	mem  int
	ord  int
	stor int
}

// MemoryRepo stores tenants + counts in a map keyed by tenant id.
type MemoryRepo struct {
	mu    sync.RWMutex
	byID  map[int64]*memoryRecord
	order []int64 // insertion order, kept sorted by id on read for determinism
}

// NewMemoryRepo constructs the repo and seeds two demo tenants (dev/test
// convenience; production uses the PG repo).
func NewMemoryRepo() *MemoryRepo {
	r := &MemoryRepo{byID: make(map[int64]*memoryRecord)}
	r.seed()
	return r
}

// seed inserts two demo tenants with realistic counts. Dev-only convenience.
func (r *MemoryRepo) seed() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r.seedInternal(Tenant{ID: 1, Name: "阳光水果行", Status: "active", Plan: "pro", CreatedAt: base}, 120, 3400, 15800, 2)
	r.seedInternal(Tenant{ID: 2, Name: "好孕母婴生活馆", Status: "active", Plan: "free", CreatedAt: base.AddDate(0, 2, 0)}, 60, 900, 4100, 1)
}

// seedInternal stores rec; caller MUST hold r.mu (or be in construction).
func (r *MemoryRepo) seedInternal(t Tenant, prod, mem, ord, stor int) {
	r.byID[t.ID] = &memoryRecord{t: t, prod: prod, mem: mem, ord: ord, stor: stor}
	r.order = append(r.order, t.ID)
}

// Seed inserts (or replaces) a tenant with explicit counts and returns it. Used
// by tests to populate additional tenants / statuses. It is NOT part of the Repo
// interface.
func (r *MemoryRepo) Seed(t Tenant, productCount, memberCount, orderCount, storeCount int) Tenant {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[t.ID]; !ok {
		r.order = append(r.order, t.ID)
	}
	r.byID[t.ID] = &memoryRecord{t: t, prod: productCount, mem: memberCount, ord: orderCount, stor: storeCount}
	return t
}

// ListAllTenants implements Repo. It returns one page of tenant summaries
// across ALL tenants matching f, ordered by id, plus the total matching count.
// It refuses to run without a platform/system Source (ensurePlatform) — a
// non-platform ctx yields an empty page (absence, never a leak — ARCH-01).
func (r *MemoryRepo) ListAllTenants(ctx context.Context, f Filter) (ListResult, error) {
	empty := ListResult{Items: []*TenantSummary{}}
	if !ensurePlatform(ctx) {
		return empty, nil
	}
	f = f.Normalize()

	r.mu.RLock()
	defer r.mu.RUnlock()

	// Deterministic order: sort tenant ids ascending.
	ids := make([]int64, len(r.order))
	copy(ids, r.order)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	// Apply filters, collecting matching records.
	matched := make([]*memoryRecord, 0, len(ids))
	for _, id := range ids {
		rec := r.byID[id]
		if rec == nil {
			continue
		}
		if f.Status != "" && rec.t.Status != f.Status {
			continue
		}
		if f.Query != "" && !strings.Contains(rec.t.Name, f.Query) {
			continue
		}
		matched = append(matched, rec)
	}

	total := int64(len(matched))
	// Page slice.
	start := f.Offset
	if start > len(matched) {
		start = len(matched)
	}
	end := start + f.Limit
	if end > len(matched) {
		end = len(matched)
	}

	items := make([]*TenantSummary, 0, end-start)
	for _, rec := range matched[start:end] {
		items = append(items, &TenantSummary{
			Tenant:       rec.t,
			ProductCount: rec.prod,
			MemberCount:  rec.mem,
			OrderCount:   rec.ord,
			StoreCount:   rec.stor,
		})
	}
	return ListResult{Items: items, Total: total}, nil
}

// ensurePlatform reports whether ctx carries a platform-level Source (system or
// admin). This is the repo-side half of the cross-tenant gate: the Service
// stamps Source=system (platformCtx); a direct caller must present a
// platform/system Source or the read is refused. A ctx with no multitenancy
// identity at all is refused too.
func ensurePlatform(ctx context.Context) bool {
	c, ok := multitenancy.FromContext(ctx)
	if !ok {
		return false
	}
	return c.Source == multitenancy.SourceSystem || c.Source == multitenancy.SourceAdmin
}

// Compile-time assertion that MemoryRepo satisfies Repo.
var _ Repo = (*MemoryRepo)(nil)
