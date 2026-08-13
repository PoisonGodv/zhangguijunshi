package loginlimit

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock is a controllable clock for deterministic tests (no sleep).
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock               { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }
func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestLimiter builds a limiter wired to a fake clock with explicit limits so
// the tests do not depend on the production defaults.
func newTestLimiter(clock *fakeClock, window time.Duration, ipMax, accMax int) *Limiter {
	return New(
		WithClock(clock.now),
		WithWindow(window),
		WithIPMax(ipMax),
		WithAccountMax(accMax),
		WithGCInterval(time.Hour), // disable GC sweep unless a test forces it
	)
}

// TestTake_AccountThreshold proves the account cap trips exactly after accMax
// allowed attempts and that the retry hint is positive.
func TestTake_AccountThreshold(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, time.Minute, 100 /*ip generous*/, 3)
	acc := "boss:1:13800000001"

	for i := 0; i < 3; i++ {
		ok, retry := l.Take("", acc) // empty IP → only account dimension enforced
		require.True(t, ok, "attempt %d must be allowed", i+1)
		assert.Equal(t, 0, retry)
	}
	// 4th attempt is over the cap.
	ok, retry := l.Take("", acc)
	require.False(t, ok, "4th attempt must be rejected")
	assert.Greater(t, retry, 0, "retry hint must be positive")
	assert.LessOrEqual(t, retry, 60, "retry hint must be within the window")
}

// TestTake_IPThreshold proves the IP cap trips independently of the account.
func TestTake_IPThreshold(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, time.Minute, 2, 100 /*account generous*/)

	// Two attempts from the same IP against DIFFERENT accounts: the IP counter
	// is shared, so the 3rd is rejected on the IP dimension even though each
	// account has only been hit once.
	for _, acc := range []string{"boss:1:111", "boss:1:222"} {
		ok, _ := l.Take("10.0.0.1", acc)
		require.True(t, ok)
	}
	ok, retry := l.Take("10.0.0.1", "boss:1:333")
	require.False(t, ok, "3rd attempt from same IP must be rejected")
	assert.Greater(t, retry, 0)
}

// TestTake_WindowRecovery proves that after the oldest event ages out of the
// window, a new attempt is allowed again (sliding window, no sleep).
func TestTake_WindowRecovery(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, time.Minute, 100, 2)
	acc := "admin:ops"

	l.Take("", acc)
	l.Take("", acc)
	ok, _ := l.Take("", acc)
	require.False(t, ok, "3rd within window rejected")

	// Advance exactly to the window boundary; the old events have expired and a
	// fresh attempt is allowed without requiring a one-second grace.
	clock.advance(time.Minute)
	ok, _ = l.Take("", acc)
	require.True(t, ok, "after the window slides, one slot is free again")
}

// TestRecordSuccess_ClearsAccount proves a successful login resets the account
// counter so the user is not locked out by their own old failures.
func TestRecordSuccess_ClearsAccount(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, time.Minute, 100, 3)
	acc := "boss:1:13800000001"

	// Burn two failures.
	l.Take("", acc)
	l.Take("", acc)
	// Success path: the handler calls RecordSuccess after a good login.
	l.RecordSuccess(acc)
	// Counter is now zero → three more attempts must be allowed.
	for i := 0; i < 3; i++ {
		ok, _ := l.Take("", acc)
		require.True(t, ok, "after RecordSuccess, attempt %d must be allowed again", i+1)
	}
}

// TestRecordSuccess_DoesNotClearIP proves the IP window is independent of login
// outcome: a success clears the account but not the IP volume counter.
func TestRecordSuccess_DoesNotClearIP(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, time.Minute, 2, 100)
	l.Take("9.9.9.9", "boss:1:a")
	l.Take("9.9.9.9", "boss:1:b")
	l.RecordSuccess("boss:1:b") // must not clear the IP window
	ok, _ := l.Take("9.9.9.9", "boss:1:c")
	require.False(t, ok, "IP window must survive a RecordSuccess on an account")
}

// TestTake_PasswordNotInKey is the security-property test: the limiter treats
// the account string as opaque, so callers MUST NOT embed the password. We
// verify two different account strings (same tenant+phone, different password
// would be a caller bug) are independent buckets — i.e. the limiter never
// merges keys by partial content.
func TestTake_AccountKeysAreOpaque(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, time.Minute, 100, 1)
	// Two distinct account keys each get their own bucket.
	ok, _ := l.Take("", "boss:1:111")
	require.True(t, ok)
	ok, _ = l.Take("", "boss:1:222")
	require.True(t, ok, "distinct account keys must not share a bucket")
}

// TestTake_EmptyKeysSkipped proves the limiter degrades safely when a dimension
// is unknown (empty IP or empty account).
func TestTake_EmptyKeysSkipped(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, time.Minute, 1, 1)
	// Both empty: no dimension enforced → always allow (degenerate but safe).
	ok, _ := l.Take("", "")
	require.True(t, ok)
}

// TestGC_DropsStaleKeys forces the GC sweep (via a short gcInterval) and proves
// stale keys are forgotten so the map cannot grow unboundedly.
func TestGC_DropsStaleKeys(t *testing.T) {
	clock := newFakeClock()
	l := New(
		WithClock(clock.now),
		WithWindow(time.Minute),
		WithIPMax(5),
		WithAccountMax(5),
		WithGCInterval(time.Millisecond), // sweep every call
	)
	l.Take("1.1.1.1", "boss:1:a")
	l.Take("2.2.2.2", "boss:1:b")
	require.Len(t, l.ipWindows, 2)
	require.Len(t, l.accWindows, 2)

	// Advance past the window; the next Take triggers a sweep (gcInterval elapsed).
	clock.advance(2 * time.Minute)
	ok, _ := l.Take("3.3.3.3", "boss:1:c")
	require.True(t, ok)
	// Stale keys gone; only the just-recorded key remains.
	assert.Len(t, l.ipWindows, 1)
	assert.Contains(t, l.ipWindows, "3.3.3.3")
	assert.Len(t, l.accWindows, 1)
	assert.Contains(t, l.accWindows, "boss:1:c")
}

// TestTake_ConcurrentSafety hammers the limiter from many goroutines; -race
// proves no data race, and the account cap is never exceeded (no more than
// accMax Taks return true for the same key).
func TestTake_ConcurrentSafety(t *testing.T) {
	clock := newFakeClock()
	l := newTestLimiter(clock, time.Minute, 1000, 10)
	acc := "boss:1:shared"

	var wg sync.WaitGroup
	var allowed int64
	var mu sync.Mutex
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _ := l.Take("10.0.0.1", acc)
			if ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Frozen clock ⇒ all attempts fall in the same window ⇒ at most accMax
	// (10) can be allowed, regardless of goroutine scheduling.
	assert.LessOrEqual(t, allowed, int64(10), "account cap must hold under concurrency")
	assert.Greater(t, allowed, int64(0), "some attempts must succeed")
}

// TestDefaults proves the documented defaults are applied when no options are
// given (guards against an accidental default change).
func TestDefaults(t *testing.T) {
	l := New()
	assert.Equal(t, DefaultWindow, l.window)
	assert.Equal(t, DefaultIPMax, l.ipMax)
	assert.Equal(t, DefaultAccountMax, l.accountMax)
	assert.Equal(t, DefaultGCInterval, l.gcInterval)
	assert.NotNil(t, l.now)
}
