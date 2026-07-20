package marketing

// MemoryCouponRepo is the in-memory, concurrency-safe CouponRepo for dev and
// tests. It enforces the same tenant-isolation contract as the production pgx
// repo: a read or mutation scoped to tenant A never sees tenant B's coupons or
// member-coupons — cross-tenant access returns ErrNotFound, indistinguishable
// from absence. All reads return defensive copies so callers cannot mutate
// stored records.
//
// IssueToMember (validate coupon → issued++ → create member-coupon) and
// RedeemAtomic (unused→used + stamp) are the two atomic critical sections,
// each serialised under the repo's single write lock so the in-memory flow has
// the atomicity the pgx implementation will get from a real transaction.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryCouponRepo stores coupons and member-coupons keyed by id, plus a
// per-tenant name index for coupon-name uniqueness / lookup.
type MemoryCouponRepo struct {
	mu        sync.RWMutex
	coupons   map[int64]*Coupon
	mc        map[int64]*MemberCoupon
	names     map[int64]map[string]int64 // tenantID -> name -> couponID
	couponSeq int64
	mcSeq     int64
}

// NewMemoryCouponRepo constructs an empty MemoryCouponRepo.
func NewMemoryCouponRepo() *MemoryCouponRepo {
	return &MemoryCouponRepo{
		coupons: make(map[int64]*Coupon),
		mc:      make(map[int64]*MemberCoupon),
		names:   make(map[int64]map[string]int64),
	}
}

// cloneCoupon returns a defensive copy of c.
func cloneCoupon(c *Coupon) *Coupon {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

// cloneMC returns a defensive copy of m.
func cloneMC(m *MemberCoupon) *MemberCoupon {
	if m == nil {
		return nil
	}
	cp := *m
	return &cp
}

// CreateCoupon persists a new coupon (id/timestamps allocated by the repo) and
// registers its name in the per-tenant uniqueness index. A duplicate name
// within the tenant yields ErrNotFound-shaped behaviour by returning the
// existing coupon's id via an error — but the contract uses GetByName for the
// uniqueness pre-check at the service layer, so here we simply insert (the
// service guarantees uniqueness before calling CreateCoupon).
func (r *MemoryCouponRepo) CreateCoupon(_ context.Context, c *Coupon) (*Coupon, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := atomic.AddInt64(&r.couponSeq, 1)
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.ID = id
	c.UpdatedAt = now
	stored := cloneCoupon(c)
	r.coupons[id] = stored
	if r.names[c.TenantID] == nil {
		r.names[c.TenantID] = make(map[string]int64)
	}
	r.names[c.TenantID][c.Name] = id
	return cloneCoupon(stored), nil
}

// GetCoupon loads a coupon by id within tenantID. ErrNotFound when absent or
// owned by another tenant.
func (r *MemoryCouponRepo) GetCoupon(_ context.Context, tenantID, id int64) (*Coupon, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.coupons[id]
	if !ok || c.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneCoupon(c), nil
}

// GetByName loads a coupon by name within tenantID. ErrNotFound when absent.
func (r *MemoryCouponRepo) GetByName(_ context.Context, tenantID int64, name string) (*Coupon, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byName, ok := r.names[tenantID]
	if !ok {
		return nil, ErrNotFound
	}
	id, ok := byName[name]
	if !ok {
		return nil, ErrNotFound
	}
	c := r.coupons[id]
	return cloneCoupon(c), nil
}

// UpdateCoupon rewrites a coupon (matched by ID + tenant). ErrNotFound when
// absent or owned by another tenant.
func (r *MemoryCouponRepo) UpdateCoupon(_ context.Context, c *Coupon) (*Coupon, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.coupons[c.ID]
	if !ok || existing.TenantID != c.TenantID {
		return nil, ErrNotFound
	}
	// Name change: update the index.
	if existing.Name != c.Name {
		delete(r.names[c.TenantID], existing.Name)
		r.names[c.TenantID][c.Name] = c.ID
	}
	c.UpdatedAt = time.Now()
	stored := cloneCoupon(c)
	r.coupons[c.ID] = stored
	return cloneCoupon(stored), nil
}

// ListCoupons returns coupons matching the filter, ordered by ID descending
// (newest first). Zero-value filter fields are not applied; Limit<=0 defaults
// to 50.
func (r *MemoryCouponRepo) ListCoupons(_ context.Context, tenantID int64, f CouponFilter) ([]*Coupon, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	var out []*Coupon
	for _, c := range r.coupons {
		if c.TenantID != tenantID {
			continue
		}
		if f.StoreID > 0 && c.StoreID != f.StoreID {
			continue
		}
		if f.Status != "" && c.Status != f.Status {
			continue
		}
		if f.Type != "" && c.Type != f.Type {
			continue
		}
		out = append(out, cloneCoupon(c))
	}
	// Sort by ID descending.
	sortCouponsByIDDesc(out)
	if f.Offset > 0 && f.Offset < len(out) {
		out = out[f.Offset:]
	} else if f.Offset >= len(out) {
		return []*Coupon{}, nil
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// IssueToMember atomically validates the coupon and creates one member-coupon.
func (r *MemoryCouponRepo) IssueToMember(_ context.Context, tenantID, memberID, couponID int64, source string, strategyID int64, expireAt *time.Time, now time.Time) (*MemberCoupon, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.coupons[couponID]
	if !ok || c.TenantID != tenantID {
		return nil, ErrNotFound
	}
	if c.Status != StatusActive {
		return nil, ErrCouponInactive
	}
	if now.Before(c.ValidFrom) || now.After(c.ValidTo) {
		return nil, ErrCouponInactive
	}
	if c.TotalQuota > 0 && c.Issued >= c.TotalQuota {
		return nil, ErrQuotaExhausted
	}
	// Increment issued on the stored record.
	c.Issued++
	c.UpdatedAt = now
	// Resolve expire: caller's expireAt, else coupon.ValidTo.
	var exp *time.Time
	if expireAt != nil {
		v := *expireAt
		exp = &v
	} else {
		v := c.ValidTo
		exp = &v
	}
	if source == "" {
		source = SourceManual
	}
	id := atomic.AddInt64(&r.mcSeq, 1)
	mc := &MemberCoupon{
		ID:         id,
		TenantID:   tenantID,
		MemberID:   memberID,
		CouponID:   couponID,
		Status:     MCStatusUnused,
		Source:     source,
		StrategyID: strategyID,
		IssuedAt:   now,
		ExpireAt:   exp,
	}
	r.mc[id] = mc
	return cloneMC(mc), nil
}

// GetMemberCoupon loads one member-coupon by id within tenantID. ErrNotFound
// when absent or owned by another tenant.
func (r *MemoryCouponRepo) GetMemberCoupon(_ context.Context, tenantID, id int64) (*MemberCoupon, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.mc[id]
	if !ok || m.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneMC(m), nil
}

// ListMemberCoupons returns a member's coupons, optionally filtered by status.
func (r *MemoryCouponRepo) ListMemberCoupons(_ context.Context, tenantID, memberID int64, status string) ([]*MemberCoupon, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*MemberCoupon
	for _, m := range r.mc {
		if m.TenantID != tenantID || m.MemberID != memberID {
			continue
		}
		if status != "" && m.Status != status {
			continue
		}
		out = append(out, cloneMC(m))
	}
	sortMCByIDDesc(out)
	return out, nil
}

// RedeemAtomic transitions a member-coupon unused→used atomically.
func (r *MemoryCouponRepo) RedeemAtomic(_ context.Context, tenantID, id, refOrderID int64, now time.Time) (*MemberCoupon, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.mc[id]
	if !ok || m.TenantID != tenantID {
		return nil, ErrNotFound
	}
	if m.Status != MCStatusUnused {
		return nil, ErrCouponUsed
	}
	// Expiry: mc.ExpireAt if set, else coupon.ValidTo.
	exp := m.ExpireAt
	if exp == nil {
		if c, ok := r.coupons[m.CouponID]; ok && c.TenantID == tenantID {
			v := c.ValidTo
			exp = &v
		}
	}
	if exp != nil && now.After(*exp) {
		m.Status = MCStatusExpired
		return nil, ErrCouponExpired
	}
	m.Status = MCStatusUsed
	m.UsedAt = &now
	m.RefOrderID = refOrderID
	return cloneMC(m), nil
}

// sortCouponsByIDDesc sorts in place by ID descending (newest first).
func sortCouponsByIDDesc(s []*Coupon) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].ID < s[j].ID; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// sortMCByIDDesc sorts in place by ID descending.
func sortMCByIDDesc(s []*MemberCoupon) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].ID < s[j].ID; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
