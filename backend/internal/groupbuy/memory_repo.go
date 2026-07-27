package groupbuy

// MemoryGroupbuyRepo is the in-memory, concurrency-safe GroupbuyRepo for dev
// and tests. It enforces the same tenant-isolation contract as the production
// pgx repo: a read or mutation scoped to tenant A never sees tenant B's
// groupbuys or participants — cross-tenant access returns ErrNotFound,
// indistinguishable from absence. All reads return defensive copies so callers
// cannot mutate stored records.
//
// JoinAtomic (validate → insert participant + joined++ + maybe success) is the
// atomic critical section, serialised under the repo's single write lock so the
// in-memory flow has the atomicity the pgx implementation will get from a real
// transaction. A joinedIndex (groupbuyID → memberID → participantID) makes the
// "already joined" precondition an O(1) check inside the critical section.

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryGroupbuyRepo stores groupbuys and participants keyed by id, plus a
// per-groupbuy participant index for the already-joined check.
type MemoryGroupbuyRepo struct {
	mu           sync.RWMutex
	groupbuys    map[int64]*Groupbuy
	participants map[int64]*Participant
	joinedIndex  map[int64]map[int64]int64 // groupbuyID -> memberID -> participantID
	gbSeq        int64
	partSeq      int64
}

// NewMemoryGroupbuyRepo constructs an empty MemoryGroupbuyRepo.
func NewMemoryGroupbuyRepo() *MemoryGroupbuyRepo {
	return &MemoryGroupbuyRepo{
		groupbuys:    make(map[int64]*Groupbuy),
		participants: make(map[int64]*Participant),
		joinedIndex:  make(map[int64]map[int64]int64),
	}
}

// cloneGroupbuy returns a defensive copy of g.
func cloneGroupbuy(g *Groupbuy) *Groupbuy {
	if g == nil {
		return nil
	}
	cp := *g
	return &cp
}

// cloneParticipant returns a defensive copy of p.
func cloneParticipant(p *Participant) *Participant {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// isAllowedGroupbuyTransition reports whether from→to is legal. Only active is
// non-terminal: active → success | failed | cancelled. The three terminal
// statuses cannot move anywhere (a succeeded squad stays succeeded, etc.).
func isAllowedGroupbuyTransition(from, to string) bool {
	if from == StatusActive {
		switch to {
		case StatusSuccess, StatusFailed, StatusCancelled:
			return true
		}
	}
	return false
}

// Create persists a new groupbuy (id/timestamps allocated by the repo).
func (r *MemoryGroupbuyRepo) Create(_ context.Context, g *Groupbuy) (*Groupbuy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := atomic.AddInt64(&r.gbSeq, 1)
	now := time.Now()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	g.ID = id
	g.UpdatedAt = now
	stored := cloneGroupbuy(g)
	r.groupbuys[id] = stored
	return cloneGroupbuy(stored), nil
}

// GetByID loads a groupbuy by id within tenantID. ErrNotFound when absent or
// owned by another tenant.
func (r *MemoryGroupbuyRepo) GetByID(_ context.Context, tenantID, id int64) (*Groupbuy, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.groupbuys[id]
	if !ok || g.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneGroupbuy(g), nil
}

// ListActive returns Status==active groupbuys for tenantID. storeID>0 filters
// by store; storeID==0 means all stores in the tenant. Ordered by ID ascending.
func (r *MemoryGroupbuyRepo) ListActive(_ context.Context, tenantID, storeID int64) ([]*Groupbuy, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Groupbuy
	for _, g := range r.groupbuys {
		if g.TenantID != tenantID || g.Status != StatusActive {
			continue
		}
		if storeID > 0 && g.StoreID != storeID {
			continue
		}
		out = append(out, cloneGroupbuy(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// UpdateStatus transitions a groupbuy's status (matched by ID + tenant) under
// the lock, re-validating the transition so a concurrent finaliser/cancel that
// raced past a ListActive sweep cannot corrupt a terminal squad. ErrNotFound
// when absent/foreign; ErrGroupbuyClosed when the transition is illegal.
func (r *MemoryGroupbuyRepo) UpdateStatus(_ context.Context, tenantID, id int64, status string, now time.Time) (*Groupbuy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groupbuys[id]
	if !ok || g.TenantID != tenantID {
		return nil, ErrNotFound
	}
	if !isAllowedGroupbuyTransition(g.Status, status) {
		return nil, ErrGroupbuyClosed
	}
	g.Status = status
	g.UpdatedAt = now
	return cloneGroupbuy(g), nil
}

// JoinAtomic is the atomic critical section (CONTRACT §11.5.4). Check order is
// deliberately: exists → already-joined → full → active. Checking "full" before
// "active" means a member joining an already-succeeded squad gets ErrGroupbuyFull
// (semantically "over-quota"), while joining a failed/cancelled squad (which by
// construction has Joined<RequiredCount) gets ErrGroupbuyClosed. On success the
// participant is inserted, Joined is incremented, and Joined>=RequiredCount
// flips the squad to success — all under the single write lock.
func (r *MemoryGroupbuyRepo) JoinAtomic(_ context.Context, tenantID, groupbuyID, memberID int64, now time.Time) (*Participant, *Groupbuy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groupbuys[groupbuyID]
	if !ok || g.TenantID != tenantID {
		return nil, nil, ErrNotFound
	}
	if _, exists := r.participantIDLocked(groupbuyID, memberID); exists {
		return nil, nil, ErrAlreadyJoined
	}
	if g.Joined >= g.RequiredCount {
		return nil, nil, ErrGroupbuyFull
	}
	if g.Status != StatusActive {
		return nil, nil, ErrGroupbuyClosed
	}
	pid := atomic.AddInt64(&r.partSeq, 1)
	p := &Participant{
		ID:         pid,
		TenantID:   tenantID,
		GroupbuyID: groupbuyID,
		MemberID:   memberID,
		Status:     PStatusJoined,
		JoinedAt:   now,
	}
	r.participants[pid] = p
	if r.joinedIndex[groupbuyID] == nil {
		r.joinedIndex[groupbuyID] = make(map[int64]int64)
	}
	r.joinedIndex[groupbuyID][memberID] = pid

	g.Joined++
	g.UpdatedAt = now
	if g.Joined >= g.RequiredCount {
		g.Status = StatusSuccess
	}
	return cloneParticipant(p), cloneGroupbuy(g), nil
}

// GetParticipant loads a participant by (groupbuyID, memberID) within tenantID.
// ErrNotFound when the squad is absent/foreign or the member never joined.
func (r *MemoryGroupbuyRepo) GetParticipant(_ context.Context, tenantID, groupbuyID, memberID int64) (*Participant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.groupbuys[groupbuyID]
	if !ok || g.TenantID != tenantID {
		return nil, ErrNotFound
	}
	pid, exists := r.participantIDLocked(groupbuyID, memberID)
	if !exists {
		return nil, ErrNotFound
	}
	return cloneParticipant(r.participants[pid]), nil
}

// ListParticipants returns every participant of a squad (matched by ID +
// tenant), ordered by participant ID ascending. ErrNotFound when the squad is
// absent/foreign.
func (r *MemoryGroupbuyRepo) ListParticipants(_ context.Context, tenantID, groupbuyID int64) ([]*Participant, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.groupbuys[groupbuyID]
	if !ok || g.TenantID != tenantID {
		return nil, ErrNotFound
	}
	var out []*Participant
	for _, pid := range r.joinedIndex[groupbuyID] {
		p := r.participants[pid]
		if p == nil || p.TenantID != tenantID {
			continue
		}
		out = append(out, cloneParticipant(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// MarkPaid transitions a participant joined→paid (payment closed-loop deferred
// to M4). Idempotent on already-paid. ErrNotFound when the squad or participant
// is absent/foreign.
func (r *MemoryGroupbuyRepo) MarkPaid(_ context.Context, tenantID, groupbuyID, memberID int64, _ time.Time) (*Participant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.groupbuys[groupbuyID]
	if !ok || g.TenantID != tenantID {
		return nil, ErrNotFound
	}
	pid, exists := r.participantIDLocked(groupbuyID, memberID)
	if !exists {
		return nil, ErrNotFound
	}
	p := r.participants[pid]
	if p.Status == PStatusJoined {
		p.Status = PStatusPaid
	}
	return cloneParticipant(p), nil
}

// participantIDLocked returns the participant id for (groupbuyID, memberID), if
// any. Must be called with r.mu (R or W) held.
func (r *MemoryGroupbuyRepo) participantIDLocked(groupbuyID, memberID int64) (int64, bool) {
	idx := r.joinedIndex[groupbuyID]
	if idx == nil {
		return 0, false
	}
	pid, ok := idx[memberID]
	return pid, ok
}
