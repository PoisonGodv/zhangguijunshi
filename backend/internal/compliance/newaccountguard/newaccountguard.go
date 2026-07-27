// Package newaccountguard implements the new-account add-friend rate limiter
// (BE-B-09, CONTRACT §11.3.3 / tech plan §4.3.4). A freshly-created WeCom
// customer-service account (wework_user_id) is capped on how many friends it may
// add per day, with the cap scaling up as the account ages: <7 days → 50,
// 7–29 days → 80, ≥30 days → 200. Exceeding the cap is a hard reject with a Decr
// rollback so a blocked attempt leaves no phantom count behind.
//
// The package is infrastructure-light and anti-corruption-clean: it imports no
// other domain package. It defines its own Counter interface with the SAME
// signature shape as freqcircuit.Counter (Incr/Decr/Get) so the main thread can
// inject one freqcircuit.MemoryCounter instance as a shared counter — but it
// does NOT import freqcircuit, keeping the dependency strictly one-way. An
// in-memory Counter implementation ships with the package for dev/tests.
//
// Tenant is always resolved from ctx via multitenancy.EnforceTenant (never from
// a request field); the counter key embeds the tenant id so two tenants' counts
// for the same wework_user_id never collide.
package newaccountguard

import (
	"context"
	"errors"
	"time"
)

// Account is the view of a WeCom customer-service account that this guard
// needs: its wework_user_id and creation timestamp. CreatedAt decides the daily
// cap via DailyCapForAge. The authoritative wework/account store lives in the
// wework domain; this package consumes it through the AccountStore interface.
type Account struct {
	WeworkUserID string
	CreatedAt    time.Time // 账号创建时间，决定 daily cap
}

// AccountStore looks up an account within a tenant. Implementations MUST treat
// an account owned by another tenant as absent (ErrAccountNotFound) — absence
// and foreign ownership are indistinguishable by design (ARCH-01).
type AccountStore interface {
	Get(ctx context.Context, tenantID int64, weworkUserID string) (*Account, error)
}

// Counter abstracts the counting primitive (Redis INCR/DECR/EXPIRE semantics).
// The signature is intentionally identical to freqcircuit.Counter so a single
// freqcircuit.MemoryCounter instance can be injected as the shared counter
// without this package importing freqcircuit.
type Counter interface {
	// Incr atomically increments key and returns the post-increment value. On
	// the first increment of a (new or expired) key it stamps the TTL to
	// expireAt and counts from 1.
	Incr(ctx context.Context, key string, expireAt time.Time) (int64, error)
	// Decr atomically decrements key (clamped at 0), rolling back a rejected
	// increment.
	Decr(ctx context.Context, key string) error
	// Get returns the current count without incrementing; an absent or expired
	// key reads as 0.
	Get(ctx context.Context, key string) (int64, error)
}

// ErrAccountNotFound is returned by AccountStore.Get when no account exists for
// the given wework_user_id within the tenant (or the account belongs to another
// tenant). AllowAddFriend surfaces it so the caller can tell an unknown account
// apart from a cap rejection.
var ErrAccountNotFound = errors.New("newaccountguard: account not found")

// Daily add-friend cap tiers (tech plan §4.3.4). Unexported; use DailyCapForAge
// to resolve the cap for a given age.
const (
	capAgeYoungDays  = 7  // age < 7 → young tier
	capAgeMatureDays = 30 // 7 ≤ age < 30 → mature tier
	capYoung         = 50
	capMature        = 80
	capOld           = 200
)
