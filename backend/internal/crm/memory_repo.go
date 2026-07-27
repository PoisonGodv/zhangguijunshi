package crm

// MemoryRepo is the in-memory, concurrency-safe Repo for dev and tests. It
// enforces the same tenant-isolation contract as the production pgx repo: a read
// or mutation scoped to tenant A never sees tenant B's groups or codes —
// cross-tenant access returns ErrNotFound, indistinguishable from absence. All
// reads return defensive copies so callers cannot mutate stored records.
//
// UpdateGroupSOP and AddGroupMembers are the two mutating critical sections; each
// is serialised under the repo's single write lock so the in-memory flow has the
// atomicity the pgx implementation will get from a single UPDATE statement.

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryRepo stores community groups and living codes keyed by id, plus a
// per-tenant code-string index for LivingCode lookup by slug.
type MemoryRepo struct {
	mu        sync.RWMutex
	groups    map[int64]*CommunityGroup
	codes     map[int64]*LivingCode
	codeIndex map[int64]map[string]int64 // tenantID -> code string -> livingCode id
	groupSeq  int64
	codeSeq   int64
}

// NewMemoryRepo constructs an empty MemoryRepo.
func NewMemoryRepo() *MemoryRepo {
	return &MemoryRepo{
		groups:    make(map[int64]*CommunityGroup),
		codes:     make(map[int64]*LivingCode),
		codeIndex: make(map[int64]map[string]int64),
	}
}

// cloneGroup returns a defensive copy of g.
func cloneGroup(g *CommunityGroup) *CommunityGroup {
	if g == nil {
		return nil
	}
	cp := *g
	return &cp
}

// cloneCode returns a defensive copy of lc. LivingCodeConfig holds only string
// fields, so a shallow copy is sufficient.
func cloneCode(lc *LivingCode) *LivingCode {
	if lc == nil {
		return nil
	}
	cp := *lc
	return &cp
}

// CreateGroup persists a new community group (id/timestamps allocated by the
// repo). An empty SOPStage is seeded to recruit so the funnel always has a
// start. The group's TenantID is trusted as-is — the service layer overwrites it
// from ctx before calling, so a caller cannot forge another tenant.
func (r *MemoryRepo) CreateGroup(_ context.Context, g *CommunityGroup) (*CommunityGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := atomic.AddInt64(&r.groupSeq, 1)
	now := time.Now()
	if g.SOPStage == "" {
		g.SOPStage = SOPRecruit
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	g.ID = id
	g.UpdatedAt = now
	stored := cloneGroup(g)
	r.groups[id] = stored
	return cloneGroup(stored), nil
}

// GetGroup loads a community group by id within tenantID. ErrNotFound when absent
// or owned by another tenant.
func (r *MemoryRepo) GetGroup(_ context.Context, tenantID, id int64) (*CommunityGroup, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.groups[id]
	if !ok || g.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneGroup(g), nil
}

// ListGroups returns the tenant's groups, optionally narrowed by store
// (storeID <= 0 = all stores), ordered by ID ascending for stable iteration.
func (r *MemoryRepo) ListGroups(_ context.Context, tenantID, storeID int64) ([]*CommunityGroup, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*CommunityGroup
	for _, g := range r.groups {
		if g.TenantID != tenantID {
			continue
		}
		if storeID > 0 && g.StoreID != storeID {
			continue
		}
		out = append(out, cloneGroup(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// UpdateGroupSOP rewrites a group's SOP stage and stamps UpdatedAt. ErrNotFound
// when the group is absent or owned by another tenant.
func (r *MemoryRepo) UpdateGroupSOP(_ context.Context, tenantID, id int64, stage string, now time.Time) (*CommunityGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groups[id]
	if !ok || g.TenantID != tenantID {
		return nil, ErrNotFound
	}
	g.SOPStage = stage
	g.UpdatedAt = now
	return cloneGroup(g), nil
}

// AddGroupMembers applies a signed delta to a group's MemberCount (clamped at 0)
// and stamps UpdatedAt. delta may be negative (members leaving). ErrNotFound when
// the group is absent or owned by another tenant.
func (r *MemoryRepo) AddGroupMembers(_ context.Context, tenantID, id int64, delta int, now time.Time) (*CommunityGroup, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groups[id]
	if !ok || g.TenantID != tenantID {
		return nil, ErrNotFound
	}
	g.MemberCount += delta
	if g.MemberCount < 0 {
		g.MemberCount = 0 // clamp — headcount never goes negative
	}
	g.UpdatedAt = now
	return cloneGroup(g), nil
}

// CreateCode persists a new living code (id/timestamp allocated by the repo) and
// registers its slug in the per-tenant code-string index.
func (r *MemoryRepo) CreateCode(_ context.Context, lc *LivingCode) (*LivingCode, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := atomic.AddInt64(&r.codeSeq, 1)
	now := time.Now()
	if lc.CreatedAt.IsZero() {
		lc.CreatedAt = now
	}
	lc.ID = id
	stored := cloneCode(lc)
	r.codes[id] = stored
	if r.codeIndex[lc.TenantID] == nil {
		r.codeIndex[lc.TenantID] = make(map[string]int64)
	}
	r.codeIndex[lc.TenantID][lc.Code] = id
	return cloneCode(stored), nil
}

// GetCode loads a living code by its slug within tenantID. ErrNotFound when
// absent or owned by another tenant.
func (r *MemoryRepo) GetCode(_ context.Context, tenantID int64, code string) (*LivingCode, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byCode, ok := r.codeIndex[tenantID]
	if !ok {
		return nil, ErrNotFound
	}
	id, ok := byCode[code]
	if !ok {
		return nil, ErrNotFound
	}
	lc := r.codes[id]
	return cloneCode(lc), nil
}

// ListCodes returns the tenant's living codes, optionally narrowed by store
// (storeID <= 0 = all stores), ordered by ID ascending.
func (r *MemoryRepo) ListCodes(_ context.Context, tenantID, storeID int64) ([]*LivingCode, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*LivingCode
	for _, lc := range r.codes {
		if lc.TenantID != tenantID {
			continue
		}
		if storeID > 0 && lc.StoreID != storeID {
			continue
		}
		out = append(out, cloneCode(lc))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
