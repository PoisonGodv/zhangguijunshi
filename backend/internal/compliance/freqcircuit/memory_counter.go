package freqcircuit

// MemoryCounter is the in-memory Counter for dev and tests. It is
// concurrency-safe (single sync.Mutex serialises all access) and lazily
// expires keys: an access past expireAt resets the value to 0. The first
// Incr of a (new or expired) key stamps the expireAt supplied by the caller.
import (
	"context"
	"sync"
	"time"
)

type counterEntry struct {
	value    int64
	expireAt time.Time
}

// MemoryCounter stores counts in a map guarded by a mutex.
type MemoryCounter struct {
	mu   sync.Mutex
	data map[string]*counterEntry
	now  func() time.Time
}

// MemoryCounterOption customises a MemoryCounter.
type MemoryCounterOption func(*MemoryCounter)

// WithMemoryNowFunc overrides the counter clock (deterministic expiry tests).
func WithMemoryNowFunc(f func() time.Time) MemoryCounterOption {
	return func(c *MemoryCounter) {
		if f != nil {
			c.now = f
		}
	}
}

// NewMemoryCounter constructs an empty MemoryCounter.
func NewMemoryCounter(opts ...MemoryCounterOption) *MemoryCounter {
	c := &MemoryCounter{
		data: make(map[string]*counterEntry),
		now:  time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// isExpired reports whether the entry's TTL has passed (relative to c.now).
func (c *MemoryCounter) isExpired(e *counterEntry) bool {
	return c.now().After(e.expireAt)
}

// Incr atomically increments key. On the first increment of a new or expired
// key, it stamps expireAt and counts from 1.
func (c *MemoryCounter) Incr(_ context.Context, key string, expireAt time.Time) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[key]
	if !ok || c.isExpired(e) {
		c.data[key] = &counterEntry{value: 1, expireAt: expireAt}
		return 1, nil
	}
	e.value++
	return e.value, nil
}

// Decr atomically decrements key, clamped at 0. An absent/expired key is a
// no-op (there is nothing to roll back).
func (c *MemoryCounter) Decr(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[key]
	if !ok || c.isExpired(e) {
		return nil
	}
	if e.value > 0 {
		e.value--
	}
	return nil
}

// Get returns the current count; an absent or expired key reads as 0.
func (c *MemoryCounter) Get(_ context.Context, key string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[key]
	if !ok || c.isExpired(e) {
		return 0, nil
	}
	return e.value, nil
}
