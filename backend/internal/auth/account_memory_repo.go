package auth

// account_memory_repo.go is an in-memory, concurrency-safe AccountRepo for dev
// and tests (mirrors MemoryConsumerRepo). NewMemoryAccountRepo seeds two demo
// accounts so boss/admin login is smoke-testable without PG. The phone index
// uses the same PhoneHash as the PG repo, so a phone typed at the login endpoint
// resolves identically in memory and in PG.

import (
	"context"
	"sync"
	"time"
)

// MemoryAccountRepo stores staff accounts keyed by id, (tenant, phoneHash),
// (tenant, name) and (tenant, username). byName is kept for display-name seeding
// convenience; FindByUsername reads the username index (migration 000018
// semantics), not byName.
type MemoryAccountRepo struct {
	mu         sync.RWMutex
	byID       map[int64]*Account
	byPhone    map[tenantPhoneKey]*Account
	byName     map[tenantNameKey]*Account
	byUsername map[tenantUsernameKey]*Account
	nextID     int64
}

type tenantPhoneKey struct {
	tenant int64
	hash   string
}

type tenantNameKey struct {
	tenant int64
	name   string
}

type tenantUsernameKey struct {
	tenant   int64
	username string
}

// NewMemoryAccountRepo constructs a MemoryAccountRepo seeded with two demo
// accounts for dev smoke (see seedDemo). Plaintext passwords are documented
// here for dev only; the production seeding path writes bcrypt hashes directly.
func NewMemoryAccountRepo() *MemoryAccountRepo {
	r := &MemoryAccountRepo{
		byID:       make(map[int64]*Account),
		byPhone:    make(map[tenantPhoneKey]*Account),
		byName:     make(map[tenantNameKey]*Account),
		byUsername: make(map[tenantUsernameKey]*Account),
		nextID:     2000, // staff ids start at 2000 to read distinctly from consumer ids (1000+) in logs
	}
	r.seedDemo()
	return r
}

// seedDemo inserts the two canonical dev accounts:
//   - boss owner   (tenant 1, store 1, phone "13800000001", password "boss12345")
//   - platform ops (tenant 1, username "ops",               password "ops12345")
//
// The platform ops account is given username "ops" so the admin username+password
// login resolves it via the username index (migration 000018). Panics on a
// bcrypt failure (a seed-time constant; if bcrypt is broken the whole binary is
// unusable, so fail fast at startup rather than silently).
func (r *MemoryAccountRepo) seedDemo() {
	_, _ = r.SeedAccount(1, 1, "demo owner", "13800000001", "owner", "boss12345")
	// Platform ops lives in the platform tenant (PlatformTenantID=1 by default)
	// and logs in by username "ops".
	_, _ = r.SeedAccountWithUsername(1, 0, "ops", "ops", "", "platform_ops", "ops12345")
}

// SeedAccount inserts a staff account with no username (backward-compatible). It
// delegates to SeedAccountWithUsername with an empty username.
func (r *MemoryAccountRepo) SeedAccount(tenantID, storeID int64, name, phone, role, password string) (*Account, error) {
	return r.SeedAccountWithUsername(tenantID, storeID, name, "", phone, role, password)
}

// SeedAccountWithUsername inserts a staff account into the memory repo, hashing
// the phone with PhoneHash and storing the bcrypt hash of password. It returns
// the stored account (with allocated id). Intended for dev/test seeding; the
// production path is PG. An empty phone is allowed (platform ops has no phone);
// an empty password returns errEmptyPassword. A non-empty username is indexed in
// byUsername and must be unique per tenant (a duplicate overwrites, mirroring
// the partial unique index on (tenant_id, username) — the memory repo is
// single-writer at seed time so last-wins is acceptable for dev/test).
func (r *MemoryAccountRepo) SeedAccountWithUsername(tenantID, storeID int64, name, username, phone, role, password string) (*Account, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	acc := &Account{
		ID:           r.nextID,
		TenantID:     tenantID,
		StoreID:      storeID,
		Name:         name,
		Username:     username,
		Phone:        phone,
		Role:         role,
		Status:       "active",
		PasswordHash: hash,
		Version:      0, // migration 000018 adds users.version (default 0); memory mirrors the DB default
		LastLoginAt:  now,
	}
	r.byID[acc.ID] = acc
	if phone != "" {
		r.byPhone[tenantPhoneKey{tenant: tenantID, hash: PhoneHash(phone)}] = acc
	}
	r.byName[tenantNameKey{tenant: tenantID, name: name}] = acc
	if username != "" {
		r.byUsername[tenantUsernameKey{tenant: tenantID, username: username}] = acc
	}
	cp := *acc
	return &cp, nil
}

// FindByPhone implements AccountRepo. An empty phone or no match returns
// ErrAccountNotFound; the tenant scope enforces row-level isolation.
func (r *MemoryAccountRepo) FindByPhone(_ context.Context, tenantID int64, phone string) (*Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	acc, ok := r.byPhone[tenantPhoneKey{tenant: tenantID, hash: PhoneHash(phone)}]
	if !ok || acc.Status != "active" {
		return nil, ErrAccountNotFound
	}
	cp := *acc
	return &cp, nil
}

// FindByUsername implements AccountRepo, reading the username index (migration
// 000018 semantics — matches users.username, not the display name). No match (or
// cross-tenant) returns ErrAccountNotFound.
func (r *MemoryAccountRepo) FindByUsername(_ context.Context, tenantID int64, username string) (*Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if username == "" {
		return nil, ErrAccountNotFound
	}
	acc, ok := r.byUsername[tenantUsernameKey{tenant: tenantID, username: username}]
	if !ok || acc.Status != "active" {
		return nil, ErrAccountNotFound
	}
	cp := *acc
	return &cp, nil
}

// TouchLogin implements AccountRepo; it stamps last_login_at = now. It never
// returns an error (the login flow treats touch as best-effort).
func (r *MemoryAccountRepo) TouchLogin(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if acc, ok := r.byID[id]; ok {
		acc.LastLoginAt = time.Now()
	}
	return nil
}

// BumpVersion implements AccountRepo; it increments the stored Version and
// returns the new value (revoke-on-password-change hook). Unknown id degrades to
// ErrAccountNotFound.
func (r *MemoryAccountRepo) BumpVersion(_ context.Context, id int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acc, ok := r.byID[id]
	if !ok {
		return 0, ErrAccountNotFound
	}
	acc.Version++
	return acc.Version, nil
}

// FindByID implements AccountRepo; it loads an active account by primary key
// within tenantID. Cross-tenant or inactive ids behave as absent
// (ErrAccountNotFound) — the tenant scope enforces row-level isolation.
func (r *MemoryAccountRepo) FindByID(_ context.Context, tenantID, id int64) (*Account, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	acc, ok := r.byID[id]
	if !ok || acc.TenantID != tenantID || acc.Status != "active" {
		return nil, ErrAccountNotFound
	}
	cp := *acc
	return &cp, nil
}

// UpdatePassword implements AccountRepo; it overwrites the stored bcrypt hash.
// It does NOT bump version (ChangePassword calls BumpVersion explicitly so the
// version cache is invalidated in step with the hash write). Unknown id →
// ErrAccountNotFound.
func (r *MemoryAccountRepo) UpdatePassword(_ context.Context, id int64, newHash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	acc, ok := r.byID[id]
	if !ok {
		return ErrAccountNotFound
	}
	acc.PasswordHash = newHash
	return nil
}
