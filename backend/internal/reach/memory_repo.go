package reach

// MemoryReachRepo is the in-memory, concurrency-safe ReachRepo for dev and
// tests. It enforces the same tenant-isolation contract as the production pgx
// repo: a read or mutation scoped to tenant A never sees tenant B's tasks,
// subscription auths, or unsubscribe records — cross-tenant access returns
// ErrNotFound, indistinguishable from absence. All reads return defensive
// copies so callers cannot mutate stored records.
//
// GrantSubscribe / ConsumeSubscribe (quota accounting) and Unsubscribe /
// Resubscribe (opt-out UNIQUE row) are the atomic critical sections, each
// serialised under the repo's single write lock so the in-memory flow has the
// atomicity the pgx implementation will get from a real transaction.

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// subKey is the composite key for a subscription authorization: one row per
// (tenant, member, template).
type subKey struct {
	tenant int64
	member int64
	tpl    string
}

// unsubKey is the composite key for an opt-out: one row per (tenant, member,
// channel).
type unsubKey struct {
	tenant  int64
	member  int64
	channel string
}

// MemoryReachRepo stores tasks by id, subscription auths and opt-outs by
// composite key, all guarded by a single mutex.
type MemoryReachRepo struct {
	mu       sync.Mutex
	tasks    map[int64]*ReachTask
	subs     map[subKey]*SubscribeAuth
	unsubs   map[unsubKey]*UnsubscribeRecord
	taskSeq  int64
	subSeq   int64
	unsubSeq int64
}

// NewMemoryReachRepo constructs an empty MemoryReachRepo.
func NewMemoryReachRepo() *MemoryReachRepo {
	return &MemoryReachRepo{
		tasks:  make(map[int64]*ReachTask),
		subs:   make(map[subKey]*SubscribeAuth),
		unsubs: make(map[unsubKey]*UnsubscribeRecord),
	}
}

// cloneTask returns a defensive copy of t.
func cloneTask(t *ReachTask) *ReachTask {
	if t == nil {
		return nil
	}
	cp := *t
	return &cp
}

// cloneSub returns a defensive copy of a.
func cloneSub(a *SubscribeAuth) *SubscribeAuth {
	if a == nil {
		return nil
	}
	cp := *a
	return &cp
}

// cloneUnsub returns a defensive copy of u.
func cloneUnsub(u *UnsubscribeRecord) *UnsubscribeRecord {
	if u == nil {
		return nil
	}
	cp := *u
	return &cp
}

// CreateTask persists a new task (id/timestamps allocated by the repo). The
// caller's TenantID is trusted because the Service always resolves it from ctx;
// the repo never invents a tenant.
func (r *MemoryReachRepo) CreateTask(_ context.Context, t *ReachTask) (*ReachTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := atomic.AddInt64(&r.taskSeq, 1)
	now := time.Now()
	t.ID = id
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.ScheduledAt.IsZero() {
		t.ScheduledAt = t.CreatedAt
	}
	stored := cloneTask(t)
	r.tasks[id] = stored
	return cloneTask(stored), nil
}

// GetTask loads a task by id within tenantID. ErrNotFound when absent or owned
// by another tenant.
func (r *MemoryReachRepo) GetTask(_ context.Context, tenantID, id int64) (*ReachTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok || t.TenantID != tenantID {
		return nil, ErrNotFound
	}
	return cloneTask(t), nil
}

// ListTasks returns tasks for tenantID, optionally narrowed by storeID (>0) and
// status (non-empty), ordered by ID descending (newest first).
func (r *MemoryReachRepo) ListTasks(_ context.Context, tenantID, storeID int64, status string) ([]*ReachTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*ReachTask
	for _, t := range r.tasks {
		if t.TenantID != tenantID {
			continue
		}
		if storeID > 0 && t.StoreID != storeID {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		out = append(out, cloneTask(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// UpdateTaskStatus rewrites a task's status / blocked_reason / sent_at (matched
// by ID + tenant). ErrNotFound when absent or owned by another tenant.
func (r *MemoryReachRepo) UpdateTaskStatus(_ context.Context, tenantID, id int64, status, blockedReason string, sentAt time.Time) (*ReachTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok || t.TenantID != tenantID {
		return nil, ErrNotFound
	}
	t.Status = status
	t.BlockedReason = blockedReason
	if !sentAt.IsZero() {
		t.SentAt = sentAt
	}
	return cloneTask(t), nil
}

// UpdateTask rewrites an existing task row matched by (t.TenantID, t.ID):
// status / blocked_reason / target_count, and sent_at when t.SentAt is non-zero
// (a zero SentAt leaves the stored value untouched, mirroring UpdateTaskStatus).
// The deferred-send worker uses it to finalize the pending row persisted at
// enqueue time instead of creating a duplicate. ErrNotFound when the row is
// absent or owned by another tenant.
func (r *MemoryReachRepo) UpdateTask(_ context.Context, t *ReachTask) (*ReachTask, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t == nil {
		return nil, ErrNotFound
	}
	stored, ok := r.tasks[t.ID]
	if !ok || stored.TenantID != t.TenantID {
		return nil, ErrNotFound
	}
	stored.Status = t.Status
	stored.BlockedReason = t.BlockedReason
	stored.TargetCount = t.TargetCount
	if !t.SentAt.IsZero() {
		stored.SentAt = t.SentAt
	}
	return cloneTask(stored), nil
}

// GrantSubscribe idempotently accumulates quota for (tenant, member, tpl): an
// existing auth gets GrantedQuota += quota (timestamps refreshed); otherwise a
// new auth is created. Returns a defensive copy.
func (r *MemoryReachRepo) GrantSubscribe(_ context.Context, tenantID, memberID int64, tplID string, quota int, now time.Time) (*SubscribeAuth, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := subKey{tenant: tenantID, member: memberID, tpl: tplID}
	a, ok := r.subs[k]
	if !ok {
		id := atomic.AddInt64(&r.subSeq, 1)
		a = &SubscribeAuth{
			ID:           id,
			TenantID:     tenantID,
			MemberID:     memberID,
			TemplateID:   tplID,
			GrantedQuota: quota,
			UsedQuota:    0,
			GrantedAt:    now,
		}
		r.subs[k] = a
		return cloneSub(a), nil
	}
	a.GrantedQuota += quota
	a.GrantedAt = now
	return cloneSub(a), nil
}

// ConsumeSubscribe atomically increments UsedQuota iff used<granted, else returns
// ErrSubscribeExhausted with no state change. An absent auth reads as
// granted=0 ⇒ exhausted.
func (r *MemoryReachRepo) ConsumeSubscribe(_ context.Context, tenantID, memberID int64, tplID string, _ time.Time) (*SubscribeAuth, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := subKey{tenant: tenantID, member: memberID, tpl: tplID}
	a, ok := r.subs[k]
	if !ok {
		return nil, ErrSubscribeExhausted
	}
	if a.UsedQuota >= a.GrantedQuota {
		return nil, ErrSubscribeExhausted
	}
	a.UsedQuota++
	return cloneSub(a), nil
}

// GetSubscribe loads the authorization for (tenant, member, tpl). ErrNotFound
// when absent.
func (r *MemoryReachRepo) GetSubscribe(_ context.Context, tenantID, memberID int64, tplID string) (*SubscribeAuth, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.subs[subKey{tenant: tenantID, member: memberID, tpl: tplID}]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneSub(a), nil
}

// Unsubscribe upserts the one (tenant, member, channel) opt-out row. A repeat
// unsubscribe refreshes the reason + timestamp.
func (r *MemoryReachRepo) Unsubscribe(_ context.Context, tenantID, memberID int64, channel, reason string, now time.Time) (*UnsubscribeRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := unsubKey{tenant: tenantID, member: memberID, channel: channel}
	u, ok := r.unsubs[k]
	if !ok {
		id := atomic.AddInt64(&r.unsubSeq, 1)
		u = &UnsubscribeRecord{
			ID:             id,
			TenantID:       tenantID,
			MemberID:       memberID,
			Channel:        channel,
			Reason:         reason,
			UnsubscribedAt: now,
		}
		r.unsubs[k] = u
		return cloneUnsub(u), nil
	}
	u.Reason = reason
	u.UnsubscribedAt = now
	return cloneUnsub(u), nil
}

// IsUnsubscribed reports whether an active opt-out row exists for (tenant,
// member, channel).
func (r *MemoryReachRepo) IsUnsubscribed(_ context.Context, tenantID, memberID int64, channel string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.unsubs[unsubKey{tenant: tenantID, member: memberID, channel: channel}]
	return ok, nil
}

// Resubscribe clears the opt-out row for (tenant, member, channel). Idempotent:
// a missing row is a no-op success.
func (r *MemoryReachRepo) Resubscribe(_ context.Context, tenantID, memberID int64, channel string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.unsubs, unsubKey{tenant: tenantID, member: memberID, channel: channel})
	return nil
}
