package auth

// version_store.go defines VersionStore: a nil-safe cache of each account's
// current token-version, so the JWT middleware can enforce the `ver` claim
// WITHOUT a per-request DB hit (CONTRACT §20.5 follow-up). The store maps
// (scene, userID) -> version. Keying matches login/middleware exactly:
//
//   - login (signStaffPair / issuePair) writes  Set(scene, accountID, version)
//     where accountID == parseSubjectID(subject) == the numeric id in "u_<id>".
//   - the middleware reads (claims.Scene, parseSubjectID(claims.Subject)) — the
//     same (scene, userID) tuple — via the VersionChecker adapter.
//
// Two implementations: MemoryVersionStore (dev / unit tests) and
// RedisVersionStore (prod, over redis.Cmdable). A nil store degrades every
// method to a no-op so the "tolerate a missing dependency" philosophy (§0)
// holds: an unwired VersionStore simply means the middleware allows every token
// (the ver check is best-effort, not a hard gate).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/redis/go-redis/v9"
)

// VersionStore is the per-(scene,userID) cache of the current token version.
//
// Get returns (0, nil) on a cache miss / unknown — 0 means "no opinion" and the
// middleware MUST treat it as allow (a cache miss must never lock a user out).
//
// Set writes the authoritative DB version (call on every login, BEFORE any
// Incr, so the cached value tracks the DB). Incr atomically bumps by one (call
// on password change). NOTE the ordering hazard: INCR on a fresh Redis key
// yields 1, NOT the DB version+1 — so login MUST Set the DB version first; Incr
// is only ever called on password change, after at least one login has seeded
// the key. Callers that Incr before Set will desync the cache from the DB (the
// cache would read 1 while the DB reads its real version); the next login's Set
// re-aligns them. This contract is documented here and exercised by the tests.
type VersionStore interface {
	Get(ctx context.Context, scene string, userID int64) (int64, error)
	Set(ctx context.Context, scene string, userID int64, version int64) error
	Incr(ctx context.Context, scene string, userID int64) (int64, error)
}

// versionKeyPrefix is the Redis key namespace for token versions. Full key:
// zg:ver:<scene>:<userID>. No TTL — the version must persist for the account's
// life; stale keys self-heal on the account's next login (Set overwrites). Key
// churn from deleted accounts is bounded and tiny (one int per account).
const versionKeyPrefix = "zg:ver:"

// versionRedisKey builds the Redis key for (scene,userID).
func versionRedisKey(scene string, userID int64) string {
	return versionKeyPrefix + scene + ":" + strconv.FormatInt(userID, 10)
}

// versionMemKey is the in-memory map key for (scene,userID). Distinct from the
// Redis key shape only by prefix; kept separate so the memory store does not
// encode Redis-specific formatting.
func versionMemKey(scene string, userID int64) string {
	return scene + ":" + strconv.FormatInt(userID, 10)
}

// MemoryVersionStore is the dev/test VersionStore: a sync-guarded map. Get on a
// miss returns (0, nil) (zero value of int64) — exactly the "no opinion" signal
// the middleware relies on.
type MemoryVersionStore struct {
	mu   sync.RWMutex
	data map[string]int64
}

// NewMemoryVersionStore constructs an empty MemoryVersionStore.
func NewMemoryVersionStore() *MemoryVersionStore {
	return &MemoryVersionStore{data: make(map[string]int64)}
}

// Get implements VersionStore. Miss → (0, nil).
func (s *MemoryVersionStore) Get(_ context.Context, scene string, userID int64) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[versionMemKey(scene, userID)], nil
}

// Set implements VersionStore.
func (s *MemoryVersionStore) Set(_ context.Context, scene string, userID int64, version int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[versionMemKey(scene, userID)] = version
	return nil
}

// Incr implements VersionStore. On a fresh key it yields 1 — see the VersionStore
// doc comment for the login-MUST-Set-first contract.
func (s *MemoryVersionStore) Incr(_ context.Context, scene string, userID int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := versionMemKey(scene, userID)
	s.data[k]++
	return s.data[k], nil
}

// RedisVersionStore backs VersionStore with Redis via redis.Cmdable. Both a real
// *redis.Client (pkg/redisx) and an in-process miniredis client (tests) satisfy
// Cmdable. A nil client degrades every method to a no-op (nil-safe §0): the
// middleware then sees a permanent cache miss and allows every token — degraded
// availability, never a lockout.
type RedisVersionStore struct {
	c redis.Cmdable
}

// NewRedisVersionStore wraps a redis.Cmdable. nil is tolerated (methods no-op).
func NewRedisVersionStore(c redis.Cmdable) *RedisVersionStore {
	return &RedisVersionStore{c: c}
}

// Get implements VersionStore. redis.Nil → (0, nil) (miss, not an error); other
// Redis errors are wrapped so a real fault is observable.
func (s *RedisVersionStore) Get(ctx context.Context, scene string, userID int64) (int64, error) {
	if s == nil || s.c == nil {
		return 0, nil
	}
	v, err := s.c.Get(ctx, versionRedisKey(scene, userID)).Int64()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, fmt.Errorf("auth: version store get: %w", err)
	}
	return v, nil
}

// Set implements VersionStore. TTL 0 = persist (no expiry).
func (s *RedisVersionStore) Set(ctx context.Context, scene string, userID int64, version int64) error {
	if s == nil || s.c == nil {
		return nil
	}
	if err := s.c.Set(ctx, versionRedisKey(scene, userID), version, 0).Err(); err != nil {
		return fmt.Errorf("auth: version store set: %w", err)
	}
	return nil
}

// Incr implements VersionStore. INCR on a missing key yields 1 — callers MUST
// have Set the DB version on login first (see VersionStore doc comment).
func (s *RedisVersionStore) Incr(ctx context.Context, scene string, userID int64) (int64, error) {
	if s == nil || s.c == nil {
		return 0, nil
	}
	v, err := s.c.Incr(ctx, versionRedisKey(scene, userID)).Result()
	if err != nil {
		return 0, fmt.Errorf("auth: version store incr: %w", err)
	}
	return v, nil
}

// VersionChecker adapts a VersionStore to the middleware's versionChecker
// surface (CurrentVersion). It exists so the middleware package need NOT import
// internal/auth (keeps the dependency direction one-way: router -> middleware,
// router -> auth; middleware stays auth-agnostic). The adapter is nil-safe at
// two levels: a nil *VersionChecker and a nil inner Store both yield (0, nil),
// so the middleware's "cache miss → allow" rule holds even when nothing is
// wired. Construct via NewVersionChecker; passing a nil Store is legal and
// produces a permanently-missing checker (every token allowed).
type VersionChecker struct{ Store VersionStore }

// NewVersionChecker wraps s in a *VersionChecker. A nil s is allowed: the
// resulting checker's CurrentVersion always returns (0, nil) (allow).
func NewVersionChecker(s VersionStore) *VersionChecker { return &VersionChecker{Store: s} }

// CurrentVersion satisfies middleware.versionChecker. nil-receiver and nil-Store
// both return (0, nil) so the middleware treats the token as fresh (allow).
func (c *VersionChecker) CurrentVersion(ctx context.Context, scene string, userID int64) (int64, error) {
	if c == nil || c.Store == nil {
		return 0, nil
	}
	return c.Store.Get(ctx, scene, userID)
}
