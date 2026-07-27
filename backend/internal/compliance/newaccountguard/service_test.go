package newaccountguard

// service_test.go covers the BE-B-09 new-account add-friend guard (CONTRACT
// §11.3.3 / §11.6 DoD). The matrix exercised:
//   - DailyCapForAge three tiers (50 / 80 / 200) + boundaries.
//   - daysSince / endOfDay helpers.
//   - AllowAddFriend: three tiers enforced, over-cap Decr rollback leaves no
//     phantom count.
//   - Cross-tenant key isolation (same wework_user_id, independent counts).
//   - AccountStore missing / foreign-tenant → ErrAccountNotFound.
//   - Clock advance → age tier jump + daily window reset (shared-clock proof).
//   - Injected fake counter records the rollback path explicitly.
//   - No tenant in ctx → error.
//
// Tests live in-package so they can read s.counter and the unexported key
// helper to prove rollback behaviour directly (same style as freqcircuit).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

func ctxWithTenant(tid int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tid,
		Source:   multitenancy.SourceSystem,
	})
}

func TestDailyCapForAge(t *testing.T) {
	s := New(NewMemoryAccountStore())
	cases := []struct {
		age  int
		want int
	}{
		{0, 50}, {1, 50}, {6, 50},
		{7, 80}, {15, 80}, {29, 80},
		{30, 200}, {100, 200},
	}
	for _, c := range cases {
		if got := s.DailyCapForAge(c.age); got != c.want {
			t.Errorf("DailyCapForAge(%d) = %d, want %d", c.age, got, c.want)
		}
	}
}

func TestDaysSince(t *testing.T) {
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		from, to time.Time
		want     int
	}{
		{base, base, 0},
		{base, base.Add(23 * time.Hour), 0},
		{base, base.Add(24 * time.Hour), 1},
		{base, base.Add(7 * 24 * time.Hour), 7},
		{base, base.Add(30 * 24 * time.Hour), 30},
		{base.Add(time.Hour), base, 0}, // future from → clamped to 0
	}
	for _, c := range cases {
		if got := daysSince(c.from, c.to); got != c.want {
			t.Errorf("daysSince(%v→%v) = %d, want %d", c.from, c.to, got, c.want)
		}
	}
}

func TestEndOfDay(t *testing.T) {
	t0 := time.Date(2026, 7, 15, 10, 30, 5, 0, time.UTC)
	got := endOfDay(t0)
	if got.Day() != 15 || got.Hour() != 23 || got.Minute() != 59 || got.Second() != 59 {
		t.Errorf("endOfDay wrong: %v (want 2026-07-15 23:59:59)", got)
	}
}

// countUntilBlocked calls AllowAddFriend until it blocks, returning the number
// of allowed calls. It fails on any non-cap error or if it never blocks.
func countUntilBlocked(t *testing.T, s *Service, ctx context.Context, uid string) int {
	t.Helper()
	n := 0
	for {
		ok, reason, err := s.AllowAddFriend(ctx, uid)
		if err != nil {
			t.Fatalf("AllowAddFriend unexpected err on call %d for %q: %v", n, uid, err)
		}
		if !ok {
			if reason != "new_account_cap" {
				t.Fatalf("%q: expected reason new_account_cap, got %q at call %d", uid, reason, n)
			}
			return n
		}
		n++
		if n > 100000 {
			t.Fatalf("%q: runaway, never blocked", uid)
		}
	}
}

func TestAllowAddFriend_ThreeTiersAndRollback(t *testing.T) {
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	store := NewMemoryAccountStore()
	store.RegisterAccount(1001, Account{WeworkUserID: "young", CreatedAt: now.Add(-1 * 24 * time.Hour)}) // age 1 → 50
	store.RegisterAccount(1001, Account{WeworkUserID: "mid", CreatedAt: now.Add(-10 * 24 * time.Hour)})  // age 10 → 80
	store.RegisterAccount(1001, Account{WeworkUserID: "old", CreatedAt: now.Add(-40 * 24 * time.Hour)})  // age 40 → 200
	s := New(store, WithNowFunc(func() time.Time { return now }))
	ctx := ctxWithTenant(1001)

	for _, tc := range []struct {
		uid     string
		wantCap int
	}{
		{"young", 50},
		{"mid", 80},
		{"old", 200},
	} {
		got := countUntilBlocked(t, s, ctx, tc.uid)
		if got != tc.wantCap {
			t.Errorf("%q: allowed %d before block, want cap %d", tc.uid, got, tc.wantCap)
		}
		// Rollback proof: after the blocked call the counter equals the cap
		// (the rejected increment was decremented back), NOT cap+1.
		count, err := s.counter.Get(ctx, addFriendKey(1001, tc.uid))
		if err != nil {
			t.Fatalf("%q: counter.Get err: %v", tc.uid, err)
		}
		if count != int64(tc.wantCap) {
			t.Errorf("%q: counter=%d after block, want %d (rollback failed)", tc.uid, count, tc.wantCap)
		}
	}
}

func TestAllowAddFriend_CrossTenantIsolation(t *testing.T) {
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	store := NewMemoryAccountStore()
	// Same wework_user_id registered under two tenants, both age 1 (cap 50).
	store.RegisterAccount(1001, Account{WeworkUserID: "wx_001", CreatedAt: now.Add(-1 * 24 * time.Hour)})
	store.RegisterAccount(1002, Account{WeworkUserID: "wx_001", CreatedAt: now.Add(-1 * 24 * time.Hour)})
	s := New(store, WithNowFunc(func() time.Time { return now }))

	ctxA := ctxWithTenant(1001)
	ctxB := ctxWithTenant(1002)

	// Tenant A exhausts its 50.
	if n := countUntilBlocked(t, s, ctxA, "wx_001"); n != 50 {
		t.Fatalf("tenant A: expected cap 50, got %d", n)
	}
	// Tenant A's next call is blocked.
	ok, reason, err := s.AllowAddFriend(ctxA, "wx_001")
	if ok || err != nil || reason != "new_account_cap" {
		t.Fatalf("tenant A post-cap should block: ok=%v reason=%q err=%v", ok, reason, err)
	}
	// Tenant B has its OWN counter — first call still allowed.
	ok, _, err = s.AllowAddFriend(ctxB, "wx_001")
	if err != nil || !ok {
		t.Fatalf("tenant B first call should be allowed (isolated counter): ok=%v err=%v", ok, err)
	}
	// Tenant B's count is 1, independent of tenant A's 50.
	got, err := s.counter.Get(ctxB, addFriendKey(1002, "wx_001"))
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("tenant B counter=%d, want 1 (independent of tenant A)", got)
	}
}

func TestAllowAddFriend_AccountMissing(t *testing.T) {
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	store := NewMemoryAccountStore()
	store.RegisterAccount(1001, Account{WeworkUserID: "known", CreatedAt: now.Add(-1 * 24 * time.Hour)})
	s := New(store, WithNowFunc(func() time.Time { return now }))
	ctx := ctxWithTenant(1001)

	// Unknown wework_user_id in this tenant.
	ok, reason, err := s.AllowAddFriend(ctx, "ghost")
	if ok {
		t.Fatal("unknown account should not be allowed")
	}
	if reason != "account_missing" {
		t.Fatalf("expected reason account_missing, got %q", reason)
	}
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound, got %v", err)
	}

	// Cross-tenant: account registered under 1002 is invisible to 1001.
	store.RegisterAccount(1002, Account{WeworkUserID: "wx_x", CreatedAt: now.Add(-1 * 24 * time.Hour)})
	ok, reason, err = s.AllowAddFriend(ctx, "wx_x")
	if ok {
		t.Fatal("foreign-tenant account should not be allowed to tenant 1001")
	}
	if reason != "account_missing" {
		t.Fatalf("foreign account: expected reason account_missing, got %q", reason)
	}
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("foreign account: expected ErrAccountNotFound, got %v", err)
	}
}

// TestAllowAddFriend_AgeTierJumpOnClockAdvance verifies that advancing the
// shared clock moves BOTH the age computation (tier jump) and the counter's
// lazy-expiry (new-day reset) in lockstep. This is the clock-sharing proof.
func TestAllowAddFriend_AgeTierJumpOnClockAdvance(t *testing.T) {
	clock := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	store := NewMemoryAccountStore()
	store.RegisterAccount(1001, Account{WeworkUserID: "grow", CreatedAt: clock})
	s := New(store, WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	// At creation (age 0): cap 50 → 50 allowed, 51st blocked.
	if n := countUntilBlocked(t, s, ctx, "grow"); n != 50 {
		t.Fatalf("age 0: expected cap 50, got %d", n)
	}

	// Advance 8 days → age 8 (cap 80) and a new calendar day (daily counter
	// lazy-expires on the next Incr). 51 calls must ALL be allowed now.
	clock = clock.Add(8 * 24 * time.Hour)
	for i := 0; i < 51; i++ {
		ok, reason, err := s.AllowAddFriend(ctx, "grow")
		if err != nil || !ok {
			t.Fatalf("age 8 call %d should be allowed under cap 80: ok=%v reason=%q err=%v", i, ok, reason, err)
		}
	}

	// Advance another 23 days → age 31 (cap 200), new day. 81 calls must all be
	// allowed under cap 200 (would have been blocked under cap 80).
	clock = clock.Add(23 * 24 * time.Hour)
	for i := 0; i < 81; i++ {
		ok, reason, err := s.AllowAddFriend(ctx, "grow")
		if err != nil || !ok {
			t.Fatalf("age 31 call %d should be allowed under cap 200: ok=%v reason=%q err=%v", i, ok, reason, err)
		}
	}
}

func TestAllowAddFriend_DailyWindowReset(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	store := NewMemoryAccountStore()
	store.RegisterAccount(1001, Account{WeworkUserID: "wx_r", CreatedAt: clock.Add(-1 * 24 * time.Hour)}) // age 1 → 50
	s := New(store, WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	// Exhaust today's cap.
	if n := countUntilBlocked(t, s, ctx, "wx_r"); n != 50 {
		t.Fatalf("expected cap 50, got %d", n)
	}
	// Same day: still blocked.
	ok, _, err := s.AllowAddFriend(ctx, "wx_r")
	if err != nil || ok {
		t.Fatalf("same-day post-cap should block: ok=%v err=%v", ok, err)
	}
	// Advance to next day: counter lazy-expires, age stays <7 (cap 50), fresh 50.
	clock = clock.Add(25 * time.Hour) // 2026-07-16 11:00 → age 2
	if n := countUntilBlocked(t, s, ctx, "wx_r"); n != 50 {
		t.Fatalf("after day rollover expected fresh cap 50, got %d", n)
	}
}

// fakeCounter records Incr/Decr/Get calls so a test can assert the rollback
// path explicitly (the shared MemoryCounter is already exercised above).
type fakeCounter struct {
	counts map[string]int64
	decrs  map[string]int
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{counts: map[string]int64{}, decrs: map[string]int{}}
}

func (f *fakeCounter) Incr(_ context.Context, key string, _ time.Time) (int64, error) {
	f.counts[key]++
	return f.counts[key], nil
}

func (f *fakeCounter) Decr(_ context.Context, key string) error {
	f.decrs[key]++
	if f.counts[key] > 0 {
		f.counts[key]--
	}
	return nil
}

func (f *fakeCounter) Get(_ context.Context, key string) (int64, error) {
	return f.counts[key], nil
}

func TestAllowAddFriend_InjectedCounterRollback(t *testing.T) {
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	store := NewMemoryAccountStore()
	store.RegisterAccount(1001, Account{WeworkUserID: "wx_f", CreatedAt: now.Add(-1 * 24 * time.Hour)}) // cap 50
	fc := newFakeCounter()
	s := New(store, WithCounter(fc), WithNowFunc(func() time.Time { return now }))
	ctx := ctxWithTenant(1001)

	for i := 0; i < 50; i++ {
		ok, _, err := s.AllowAddFriend(ctx, "wx_f")
		if err != nil || !ok {
			t.Fatalf("call %d should allow: ok=%v err=%v", i, ok, err)
		}
	}
	// 51st: over cap → exactly one Decr rollback, then blocked.
	ok, reason, err := s.AllowAddFriend(ctx, "wx_f")
	if ok || err != nil || reason != "new_account_cap" {
		t.Fatalf("51st should block new_account_cap: ok=%v reason=%q err=%v", ok, reason, err)
	}
	key := addFriendKey(1001, "wx_f")
	if fc.decrs[key] != 1 {
		t.Errorf("expected exactly 1 Decr on cap-exceed, got %d", fc.decrs[key])
	}
	if fc.counts[key] != 50 {
		t.Errorf("count after rollback should be 50, got %d", fc.counts[key])
	}
}

func TestAllowAddFriend_NoTenant(t *testing.T) {
	s := New(NewMemoryAccountStore(), WithNowFunc(func() time.Time {
		return time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	}))
	ok, _, err := s.AllowAddFriend(context.Background(), "wx_001")
	if err == nil {
		t.Fatal("expect ErrNoTenant without tenant in ctx")
	}
	if ok {
		t.Fatal("should not allow without tenant")
	}
}
