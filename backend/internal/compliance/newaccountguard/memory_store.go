package newaccountguard

// MemoryAccountStore is the in-memory, concurrency-safe AccountStore for dev
// and tests. Accounts are keyed by tenant → wework_user_id. Get enforces tenant
// isolation: an account registered under tenant A is invisible to tenant B
// (returns ErrAccountNotFound), indistinguishable from absence. All reads
// return a defensive copy so callers cannot mutate stored records.

import (
	"context"
	"sync"
)

// MemoryAccountStore stores accounts per tenant.
type MemoryAccountStore struct {
	mu       sync.RWMutex
	accounts map[int64]map[string]Account // tenantID -> weworkUserID -> Account
}

// Compile-time assertion that *MemoryAccountStore satisfies AccountStore.
var _ AccountStore = (*MemoryAccountStore)(nil)

// NewMemoryAccountStore constructs an empty MemoryAccountStore.
func NewMemoryAccountStore() *MemoryAccountStore {
	return &MemoryAccountStore{
		accounts: make(map[int64]map[string]Account),
	}
}

// RegisterAccount seeds an account for a tenant (test/dev helper). It is NOT
// part of the AccountStore interface and intentionally takes tenantID directly:
// it is a raw seed, not a tenant-guarded entry point, so the EnforceTenant rule
// for repo/service methods does not apply here. Re-registering the same
// wework_user_id within a tenant overwrites the previous record (last write
// wins), which lets a test move an account's CreatedAt to exercise age tiers.
func (s *MemoryAccountStore) RegisterAccount(tenantID int64, account Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accounts[tenantID] == nil {
		s.accounts[tenantID] = make(map[string]Account)
	}
	// Account holds only value types (string + time.Time), so a plain assignment
	// is already a copy; write it through to detach the caller's struct.
	s.accounts[tenantID][account.WeworkUserID] = account
}

// Get returns the account for weworkUserID within tenantID. ErrAccountNotFound
// when the account is absent OR owned by another tenant (cross-tenant reads are
// reported as absence by design).
func (s *MemoryAccountStore) Get(_ context.Context, tenantID int64, weworkUserID string) (*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byUser, ok := s.accounts[tenantID]
	if !ok {
		return nil, ErrAccountNotFound
	}
	acct, ok := byUser[weworkUserID]
	if !ok {
		return nil, ErrAccountNotFound
	}
	cp := acct // defensive copy
	return &cp, nil
}
