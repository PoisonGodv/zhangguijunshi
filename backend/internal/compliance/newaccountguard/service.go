package newaccountguard

// Service.AllowAddFriend implements the new-account add-friend rate limit
// (BE-B-09, CONTRACT §11.3.3). The guard resolves the tenant from ctx, looks up
// the wework account, computes its age, picks the daily cap, and increments a
// per-day counter; a count over the cap is rolled back with Decr and rejected.
//
// Clock sharing: New builds the default MemoryCounter AFTER options are applied
// and shares the service's injected clock with it (WithMemoryNowFunc(s.now)) —
// the same pitfall freqcircuit solves the same way. Advancing a shared clock in
// a test moves both the age computation and the counter's lazy-expiry in
// lockstep; without the share, the counter would expire on wall-clock time while
// the service aged on the injected clock, diverging silently.

import (
	"context"
	"fmt"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service is the new-account add-friend guard.
type Service struct {
	store   AccountStore
	counter Counter
	now     func() time.Time
}

// Option customises a Service.
type Option func(*Service)

// WithCounter overrides the default MemoryCounter. The main thread may inject a
// shared freqcircuit.MemoryCounter (it satisfies Counter) so the new-account
// guard and the frequency circuit share one counter store. A nil argument is
// ignored (keeps the default).
func WithCounter(c Counter) Option {
	return func(s *Service) {
		if c != nil {
			s.counter = c
		}
	}
}

// WithNowFunc overrides the service clock (deterministic age / window tests). A
// nil argument is ignored.
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// New constructs a Service backed by store. The default counter is a
// MemoryCounter created AFTER options are applied, sharing the service's
// (possibly injected) clock so age and lazy-expiry stay in sync.
func New(store AccountStore, opts ...Option) *Service {
	s := &Service{store: store, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if s.counter == nil {
		s.counter = NewMemoryCounter(WithMemoryNowFunc(s.now))
	}
	return s
}

// addFriendKey is the Redis-shape counter key:
//
//	t:{tid}:freq:add_friend:{weworkUserID}:daily
//
// The tenant id is embedded so two tenants' counts for the same wework_user_id
// never collide (cross-tenant key isolation).
func addFriendKey(tenantID int64, weworkUserID string) string {
	return fmt.Sprintf("t:%d:freq:add_friend:%s:daily", tenantID, weworkUserID)
}

// daysSince returns the number of full 24h days between from and to (≥0). A
// future `from` reads as 0 (defensive against clock skew / bad CreatedAt).
func daysSince(from, to time.Time) int {
	diff := to.Sub(from)
	if diff < 0 {
		return 0
	}
	return int(diff / (24 * time.Hour))
}

// endOfDay returns 23:59:59.999999999 of t's calendar day in t's location — the
// TTL stamped on the daily counter so a count naturally expires at day end.
func endOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 23, 59, 59, 999999999, t.Location())
}

// DailyCapForAge returns the per-day add-friend cap for an account of ageDays
// full days: <7 → 50, 7–29 → 80, ≥30 → 200 (tech plan §4.3.4). Exposed for
// tests and observation.
func (s *Service) DailyCapForAge(ageDays int) int {
	switch {
	case ageDays < capAgeYoungDays:
		return capYoung
	case ageDays < capAgeMatureDays:
		return capMature
	default:
		return capOld
	}
}

// AllowAddFriend reports whether the wework account may add one more friend
// today. Flow:
//  1. EnforceTenant → tenantID (no tenant → propagate the error).
//  2. AccountStore.Get → account. Missing/foreign → (false, "account_missing",
//     ErrAccountNotFound): the cap cannot be determined for an unknown account,
//     so the add is blocked AND the sentinel is surfaced so the caller can tell
//     it apart from a cap rejection.
//  3. age = daysSince(CreatedAt, now); cap = DailyCapForAge(age).
//  4. Incr the daily counter (key = t:{tid}:freq:add_friend:{uid}:daily, TTL =
//     end of today).
//  5. count > cap → Decr rollback (no phantom count) + (false,
//     "new_account_cap", nil); else (true, "", nil).
func (s *Service) AllowAddFriend(ctx context.Context, weworkUserID string) (allowed bool, reason string, err error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return false, "", err
	}
	account, err := s.store.Get(ctx, tenantID, weworkUserID)
	if err != nil {
		return false, "account_missing", err
	}

	now := s.now()
	age := daysSince(account.CreatedAt, now)
	dailyCap := int64(s.DailyCapForAge(age))

	key := addFriendKey(tenantID, weworkUserID)
	count, err := s.counter.Incr(ctx, key, endOfDay(now))
	if err != nil {
		return false, "", err
	}
	if count > dailyCap {
		// Over cap: roll back this increment so a rejected attempt leaves no
		// phantom count, then block (nil error — this is a guard decision, not
		// a system fault).
		_ = s.counter.Decr(ctx, key)
		return false, "new_account_cap", nil
	}
	return true, "", nil
}
