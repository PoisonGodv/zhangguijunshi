// Package loginlimit is a process-internal sliding-window rate limiter for the
// boss/admin password-login endpoints (MVP login brute-force hardening).
//
// It limits by TWO dimensions simultaneously:
//   - client IP:   a cap on raw attempts from one source (stops an attacker
//     rotating across many accounts from a single IP).
//   - account key: a cap on attempts against one identity. For boss this is
//     tenant_id + phone; for admin it is the username. The password is NEVER
//     part of the key (and never appears in any log) — keys are opaque strings
//     supplied by the caller. The account window is cleared on a successful
//     login (RecordSuccess) so a legitimate user who mistypes a few times and
//     then remembers the password is not locked out by their own old failures.
//
// MVP runs IN-PROCESS (a single api replica). Multi-replica deployments MUST
// swap this for a Redis-backed shared limiter (e.g. INCR + EXPIRE per key per
// window) — see docs/runbooks/deploy.md "安全红线". A per-process limiter
// under-counts when N replicas each allow up to <max> per window.
//
// The limiter is concurrency-safe (single mutex) and lazily garbage-collects
// stale keys so the map cannot grow unboundedly under a rotating-key attack.
package loginlimit

import (
	"sync"
	"time"
)

// Default policy tuned for MVP. A single human types at most a few attempts per
// minute; 5/account/min tolerates a forgotten-then-remembered password while
// making online bcrypt brute force impractical. 20/ip/min lets a small office
// behind NAT log in without tripping; an attacker from one IP is capped well
// below the throughput bcrypt-cost online cracking would need.
const (
	DefaultWindow     = time.Minute
	DefaultIPMax      = 20
	DefaultAccountMax = 5
	DefaultGCInterval = 5 * time.Minute
)

// Limiter is a concurrency-safe sliding-window login rate limiter.
type Limiter struct {
	now        func() time.Time
	window     time.Duration
	ipMax      int
	accountMax int
	gcInterval time.Duration

	mu         sync.Mutex
	ipWindows  map[string][]time.Time
	accWindows map[string][]time.Time
	lastGC     time.Time
}

// Option configures a Limiter.
type Option func(*Limiter)

// WithClock injects a clock (tests). Default time.Now. Tests MUST use this so
// they advance the window deterministically instead of sleeping.
func WithClock(f func() time.Time) Option { return func(l *Limiter) { l.now = f } }

// WithWindow sets the sliding window length. Must be > 0.
func WithWindow(d time.Duration) Option { return func(l *Limiter) { l.window = d } }

// WithIPMax sets the per-IP attempt cap per window. Must be > 0.
func WithIPMax(n int) Option { return func(l *Limiter) { l.ipMax = n } }

// WithAccountMax sets the per-account attempt cap per window. Must be > 0.
func WithAccountMax(n int) Option { return func(l *Limiter) { l.accountMax = n } }

// WithGCInterval sets how often stale keys are swept. Must be > 0.
func WithGCInterval(d time.Duration) Option { return func(l *Limiter) { l.gcInterval = d } }

// New constructs a Limiter with the default policy applied, then the given
// options. A zero-valued Option target is impossible because every field has a
// sane default set first; options only override.
func New(opts ...Option) *Limiter {
	l := &Limiter{
		now:        time.Now,
		window:     DefaultWindow,
		ipMax:      DefaultIPMax,
		accountMax: DefaultAccountMax,
		gcInterval: DefaultGCInterval,
		ipWindows:  make(map[string][]time.Time),
		accWindows: make(map[string][]time.Time),
	}
	for _, o := range opts {
		o(l)
	}
	l.lastGC = l.now()
	return l
}

// Take atomically checks both the IP and the account windows and, if both are
// within their limits, records the attempt. Returns (true, 0) when the attempt
// may proceed.
//
// When either window is at/over its cap it returns (false, retryAfterSeconds)
// and does NOT record the attempt — a rejected request must NOT extend the
// lockout, otherwise a trickle attack (one request per second) would keep the
// account locked forever. retryAfterSeconds is the wait until the oldest
// offending event ages out of the window; it is at least 1 so the Retry-After
// HTTP header is always meaningful.
//
// An empty ip or account skips that dimension (only the other is enforced);
// the caller decides what to feed in (it is safe to pass "" if a dimension is
// unknown).
func (l *Limiter) Take(ip, account string) (bool, int) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.gcLocked(now)

	ipOver, ipRetry := false, 0
	if ip != "" && l.ipMax > 0 {
		ipOver, ipRetry = l.checkLocked(l.ipWindows, ip, now, l.ipMax)
	}
	accOver, accRetry := false, 0
	if account != "" && l.accountMax > 0 {
		accOver, accRetry = l.checkLocked(l.accWindows, account, now, l.accountMax)
	}
	if ipOver || accOver {
		retry := ipRetry
		if accRetry > retry {
			retry = accRetry
		}
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	// Both within limit: record the attempt in each enforced dimension.
	if ip != "" && l.ipMax > 0 {
		l.ipWindows[ip] = append(l.ipWindows[ip], now)
	}
	if account != "" && l.accountMax > 0 {
		l.accWindows[account] = append(l.accWindows[account], now)
	}
	return true, 0
}

// RecordSuccess clears the account's attempt window after a successful login.
// This is what keeps a legitimate user from being locked out by their own old
// failures once they finally type the right password: the moment login
// succeeds, the account counter resets to zero. The IP window is NOT cleared
// (it tracks raw request volume, independent of outcome). Safe to call on an
// unknown account (no-op).
func (l *Limiter) RecordSuccess(account string) {
	if account == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.accWindows, account)
}

// checkLocked reports whether key is at/over max within the window and, if so,
// the seconds-to-wait. It ALSO trims events older than the window from the
// stored slice (so each key's slice stays tight without waiting for gcLocked).
// The map is mutated under the caller's lock.
//
// Because every allowed Take appends exactly one event and a rejected Take
// appends none, the in-window count for a key is at most max between GC runs;
// the slice length is therefore bounded by max + (events arriving during one
// window before the oldest expires), which stays small.
func (l *Limiter) checkLocked(windows map[string][]time.Time, key string, now time.Time, max int) (bool, int) {
	events := windows[key]
	if len(events) == 0 {
		return false, 0
	}
	cutoff := now.Add(-l.window)
	// Trim leading events that aged out (slice is append-ordered ascending).
	// An event exactly one window old is expired: the policy is [now-window, now)
	// for previous attempts, so a request at t0+window gets a fresh slot.
	keep := 0
	for keep < len(events) && !events[keep].After(cutoff) {
		keep++
	}
	if keep > 0 {
		events = events[keep:]
		windows[key] = events
	}
	// count == len(events) now (all remaining are within the window).
	if len(events) < max {
		return false, 0
	}
	// At/over cap. The oldest in-window event must age out for the count to
	// drop below max; that is events[0] + window. Round up to the next second.
	retry := events[0].Add(l.window).Sub(now)
	if retry < time.Second {
		retry = time.Second
	}
	return true, int((retry + time.Second - 1) / time.Second)
}

// gcLocked drops keys whose most-recent event is older than the window (the key
// is fully stale — safe to forget). Called lazily on each Take at most once per
// gcInterval, so a hot loop does not pay the sweep cost every call while a cold
// overnight attack still gets periodic cleanup.
func (l *Limiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < l.gcInterval {
		return
	}
	l.lastGC = now
	cutoff := now.Add(-l.window)
	for key, events := range l.ipWindows {
		if len(events) == 0 || !events[len(events)-1].After(cutoff) {
			delete(l.ipWindows, key)
		}
	}
	for key, events := range l.accWindows {
		if len(events) == 0 || !events[len(events)-1].After(cutoff) {
			delete(l.accWindows, key)
		}
	}
}
