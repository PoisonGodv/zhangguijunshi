package jwt

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestSessionStoreContract exercises the full SessionStore contract against
// both implementations so memory and redis stay behaviour-equivalent.
func TestSessionStoreContract(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		runStoreContract(t, func() (SessionStore, func()) {
			s := NewMemorySessionStore()
			return s, func() { _ = s.Close() }
		})
	})
	t.Run("redis", func(t *testing.T) {
		runStoreContract(t, func() (SessionStore, func()) {
			mr := miniredis.RunT(t)
			c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			return NewRedisSessionStore(c), func() { _ = c.Close() }
		})
	})
}

func runStoreContract(t *testing.T, newStore func() (SessionStore, func())) {
	t.Helper()
	store, cleanup := newStore()
	defer cleanup()
	ctx := context.Background()

	in := RefreshData{
		TenantID: 50021, StoreID: 883, UserID: 100234, Subject: "u_100234",
		Scene: SceneConsumer, Role: "consumer", OpenID: "o_xxx", Version: 1, DeviceID: "dev_1",
	}

	// Issue + Get round-trip preserves every field.
	tok, err := store.IssueRefresh(ctx, in, time.Hour)
	if err != nil {
		t.Fatalf("IssueRefresh: %v", err)
	}
	if tok == "" {
		t.Fatal("empty refresh token")
	}
	got, err := store.GetRefresh(ctx, tok)
	if err != nil {
		t.Fatalf("GetRefresh: %v", err)
	}
	if got != in {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, in)
	}

	// Unknown token -> ErrRefreshNotFound.
	if _, err := store.GetRefresh(ctx, "nope"); err != ErrRefreshNotFound {
		t.Fatalf("expected ErrRefreshNotFound, got %v", err)
	}

	// Revoke -> not found.
	if err := store.RevokeRefresh(ctx, tok); err != nil {
		t.Fatalf("RevokeRefresh: %v", err)
	}
	if _, err := store.GetRefresh(ctx, tok); err != ErrRefreshNotFound {
		t.Fatalf("expected ErrRefreshNotFound after revoke, got %v", err)
	}

	// jti blacklist: unknown -> false; blocked -> true.
	if blocked, err := store.IsBlocked(ctx, "jti-1"); err != nil || blocked {
		t.Fatalf("unknown jti should not be blocked (blocked=%v err=%v)", blocked, err)
	}
	if err := store.BlockJTI(ctx, "jti-1", time.Hour); err != nil {
		t.Fatalf("BlockJTI: %v", err)
	}
	if blocked, err := store.IsBlocked(ctx, "jti-1"); err != nil || !blocked {
		t.Fatalf("blocked jti should report true (blocked=%v err=%v)", blocked, err)
	}
}

func TestRedisSessionStore_TTLExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer c.Close()
	store := NewRedisSessionStore(c)
	ctx := context.Background()

	tok, err := store.IssueRefresh(ctx, RefreshData{Subject: "u"}, 2*time.Second)
	if err != nil {
		t.Fatalf("IssueRefresh: %v", err)
	}
	if err := store.BlockJTI(ctx, "jti-x", 2*time.Second); err != nil {
		t.Fatalf("BlockJTI: %v", err)
	}

	// Fast-forward past TTL without real sleeping (miniredis virtual clock).
	mr.FastForward(3 * time.Second)

	if _, err := store.GetRefresh(ctx, tok); err != ErrRefreshNotFound {
		t.Fatalf("expected refresh expiry, got %v", err)
	}
	if blocked, err := store.IsBlocked(ctx, "jti-x"); err != nil || blocked {
		t.Fatalf("expected jti to expire (blocked=%v err=%v)", blocked, err)
	}
}

func TestRedisSessionStore_NilClientPanicsOnUse(t *testing.T) {
	// NewRedisSessionStore(nil) is allowed (lazy); using it surfaces a nil-deref
	// from go-redis. We only assert construction does not panic and that the
	// type implements the interface.
	var _ SessionStore = NewRedisSessionStore(nil)
}
