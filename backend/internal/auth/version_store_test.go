package auth

// version_store_test.go exercises both VersionStore implementations + the
// VersionChecker adapter (CONTRACT §20.5). Memory is a pure round-trip; Redis
// uses miniredis so the test runs without a live Redis. The nil-client paths
// prove the §0 nil-safe contract (a missing Redis degrades to no-op, never a
// panic or lockout). The Set-before-Incr contract is asserted explicitly:
// INCR on a fresh key yields 1, NOT dbVersion+1.

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestMemoryVersionStore_GetSetIncr(t *testing.T) {
	s := NewMemoryVersionStore()
	ctx := context.Background()

	// Miss -> (0, nil); the "no opinion" signal the middleware allows on.
	v, err := s.Get(ctx, "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(0), v)

	// Set writes the authoritative DB version (the login path).
	require.NoError(t, s.Set(ctx, "boss", 2001, 5))
	v, err = s.Get(ctx, "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(5), v)

	// Incr AFTER Set bumps from the DB baseline (5 -> 6), matching a DB bump.
	v, err = s.Incr(ctx, "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(6), v)
	v, err = s.Get(ctx, "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(6), v)

	// Keying is per-(scene,userID): a different scene or user is a separate miss.
	require.NoError(t, s.Set(ctx, "boss", 2002, 9))
	v, _ = s.Get(ctx, "boss", 2002)
	require.Equal(t, int64(9), v)
	v, _ = s.Get(ctx, "admin", 2001)
	require.Equal(t, int64(0), v, "scene is part of the key")
}

// TestMemoryVersionStore_IncrOnFreshKeyYieldsOne documents the hazard: INCR on a
// key that was never Set returns 1, NOT the real DB version+1. Login MUST Set
// first. ChangePassword only ever Incrs after a login has seeded the key, so the
// happy path is unaffected; this test pins the contract so a future caller that
// Incrs-before-Set is caught in review.
func TestMemoryVersionStore_IncrOnFreshKeyYieldsOne(t *testing.T) {
	s := NewMemoryVersionStore()
	v, err := s.Incr(context.Background(), "boss", 9999)
	require.NoError(t, err)
	require.Equal(t, int64(1), v, "INCR on a fresh key yields 1, not dbVersion+1")
}

func TestRedisVersionStore_GetSetIncr(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()
	s := NewRedisVersionStore(c)
	ctx := context.Background()

	// Miss -> redis.Nil -> (0, nil).
	v, err := s.Get(ctx, "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(0), v)

	// Set then Get round-trip; verify the exact key shape landed in Redis.
	require.NoError(t, s.Set(ctx, "boss", 2001, 5))
	v, err = s.Get(ctx, "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(5), v)
	require.True(t, mr.Exists("zg:ver:boss:2001"), "key must be zg:ver:<scene>:<userID>")

	// Incr after Set bumps the cached baseline (5 -> 6).
	v, err = s.Incr(ctx, "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(6), v)

	// Cross-(scene,user) isolation: admin/2001 is a separate miss.
	v, _ = s.Get(ctx, "admin", 2001)
	require.Equal(t, int64(0), v)

	// The value persisted with no TTL (no expiry). miniredis TTL() returns 0 for
	// a key with no expiry (and -1s for a real Redis no-TTL in some builds; both
	// mean "persists").
	ttl := mr.TTL("zg:ver:boss:2001")
	require.True(t, ttl <= 0, "version keys persist (no positive TTL), got %v", ttl)
}

func TestRedisVersionStore_IncrOnFreshKeyYieldsOne(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()
	s := NewRedisVersionStore(c)
	v, err := s.Incr(context.Background(), "boss", 9999)
	require.NoError(t, err)
	require.Equal(t, int64(1), v, "INCR on a fresh key yields 1 — login MUST Set first")
}

// TestRedisVersionStore_NilClientIsSafe proves the §0 nil-safe contract: a nil
// redis.Cmdable degrades every method to a no-op (miss / nil), never a panic.
// This is the degraded mode when Redis is absent in local dev.
func TestRedisVersionStore_NilClientIsSafe(t *testing.T) {
	var nilStore *RedisVersionStore // nil pointer — methods must not panic
	v, err := nilStore.Get(context.Background(), "boss", 1)
	require.NoError(t, err)
	require.Equal(t, int64(0), v)
	require.NoError(t, nilStore.Set(context.Background(), "boss", 1, 5))
	v, err = nilStore.Incr(context.Background(), "boss", 1)
	require.NoError(t, err)
	require.Equal(t, int64(0), v)

	// Constructed-with-nil client is also safe.
	s := NewRedisVersionStore(nil)
	v, err = s.Get(context.Background(), "boss", 1)
	require.NoError(t, err)
	require.Equal(t, int64(0), v)
	require.NoError(t, s.Set(context.Background(), "boss", 1, 5))
	v, err = s.Incr(context.Background(), "boss", 1)
	require.NoError(t, err)
	require.Equal(t, int64(0), v)
}

// TestVersionChecker_AdaptsAndIsNilSafe proves the adapter the router hands to
// the middleware: it delegates Get to CurrentVersion, and a nil Store (or nil
// receiver) yields (0, nil) so the middleware allows.
func TestVersionChecker_AdaptsAndIsNilSafe(t *testing.T) {
	store := NewMemoryVersionStore()
	_ = store.Set(context.Background(), "boss", 2001, 7)

	checker := NewVersionChecker(store)
	v, err := checker.CurrentVersion(context.Background(), "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(7), v)

	// Nil Store -> (0, nil) (allow) — the unwired case.
	emptyChecker := NewVersionChecker(nil)
	v, err = emptyChecker.CurrentVersion(context.Background(), "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(0), v)

	// Nil receiver is also safe.
	var nilChecker *VersionChecker
	v, err = nilChecker.CurrentVersion(context.Background(), "boss", 2001)
	require.NoError(t, err)
	require.Equal(t, int64(0), v)
}
