package auth

// MemoryConsumerRepo is an in-memory, concurrency-safe ConsumerRepo for dev and
// tests. It mirrors the production uniqueness contract: (tenant_id, openid) is
// unique, and ids are allocated monotonically per process.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// MemoryConsumerRepo stores consumers keyed by (tenantID, openid).
type MemoryConsumerRepo struct {
	mu       sync.RWMutex
	byOpenID map[tenantOpenID]*Consumer
	byID     map[int64]*Consumer
	nextID   atomic.Int64
}

type tenantOpenID struct {
	tenant int64
	openid string
}

// NewMemoryConsumerRepo constructs an empty repo.
func NewMemoryConsumerRepo() *MemoryConsumerRepo {
	r := &MemoryConsumerRepo{
		byOpenID: make(map[tenantOpenID]*Consumer),
		byID:     make(map[int64]*Consumer),
	}
	r.nextID.Store(1000) // consumer ids start above 1000 to look realistic in logs
	return r
}

// UpsertByOpenID implements ConsumerRepo.
func (r *MemoryConsumerRepo) UpsertByOpenID(_ context.Context, tenantID int64, openid, unionid, sessionKey string) (*Consumer, error) {
	key := tenantOpenID{tenant: tenantID, openid: openid}
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.byOpenID[key]; ok {
		existing.UnionID = unionid
		existing.SessionKey = sessionKey
		existing.UpdatedAt = now
		// Return a defensive copy so callers cannot mutate the stored record.
		cp := *existing
		return &cp, nil
	}

	c := &Consumer{
		ID:         r.nextID.Add(1),
		TenantID:   tenantID,
		OpenID:     openid,
		UnionID:    unionid,
		SessionKey: sessionKey,
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	r.byOpenID[key] = c
	r.byID[c.ID] = c
	cp := *c
	return &cp, nil
}

// GetByID implements ConsumerRepo.
func (r *MemoryConsumerRepo) GetByID(_ context.Context, tenantID, id int64) (*Consumer, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byID[id]
	if !ok || c.TenantID != tenantID {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}
