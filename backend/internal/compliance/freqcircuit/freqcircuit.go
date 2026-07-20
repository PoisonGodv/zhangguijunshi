// Package freqcircuit implements the reach-frequency circuit breaker (BE-B-04,
// CONTRACT §10.3 / tech plan §4.3.3). It enforces the WeCom / SMS frequency
// caps that the plan-book makes product-level mandatory (不可关闭): WeCom group
// broadcasts and 1v1 messages are capped at 1/day, 7/week, and N/month where N
// is the number of days in the current month.
//
// The package is infrastructure-light: a Counter interface abstracts the
// Redis INCR/DECR/EXPIRE semantics, with a concurrency-safe in-memory
// implementation for dev and tests. The production Redis implementation drops
// in later without touching the Service. Tenant is always resolved from ctx
// (never from a request field); the key shape is t:{tid}:freq:{ch}:{period}.
package freqcircuit

import (
	"context"
	"errors"
	"time"
)

// Channel is a reach channel. Only WeCom channels are frequency-limited by
// default; subscribe_msg / sms are unlimited (absent from the default limits
// map).
type Channel string

const (
	ChannelWeworkGroup  Channel = "wework_group"
	ChannelWeworkOneV   Channel = "wework_1v1"
	ChannelSubscribeMsg Channel = "subscribe_msg"
	ChannelSMS          Channel = "sms"
)

// Period is one of the three counting windows.
type Period string

const (
	PeriodDaily   Period = "daily"
	PeriodWeekly  Period = "weekly"
	PeriodMonthly Period = "monthly"
)

// Limit is the per-channel cap. Monthly=0 has a special meaning: "the monthly
// cap equals the number of days in the current month" (计划书 口径). A channel
// entirely absent from the limits map is unlimited.
type Limit struct {
	Daily, Weekly, Monthly int64
}

// Counter abstracts the counting primitive. The in-memory implementation is
// concurrency-safe and lazily expires keys; a future Redis implementation maps
// Incr→INCR+EXPIREAT, Decr→DECR, Get→GET.
type Counter interface {
	// Incr atomically increments key and returns the post-increment value. On
	// the first increment of a key (or after expiry) it sets the TTL to expireAt.
	Incr(ctx context.Context, key string, expireAt time.Time) (int64, error)
	// Decr atomically decrements key (clamped at 0), rolling back a rejected
	// increment.
	Decr(ctx context.Context, key string) error
	// Get returns the current count without incrementing; an expired or absent
	// key reads as 0.
	Get(ctx context.Context, key string) (int64, error)
}

// ErrFreqExceeded is returned by callers that prefer an error form; Service.Allow
// instead returns (false, reason) so the reach layer can branch on the reason.
var ErrFreqExceeded = errors.New("freqcircuit: frequency exceeded")

// Service is the frequency circuit breaker. It holds a Counter, an injected
// clock, and the per-channel limit table.
type Service struct {
	counter Counter
	now     func() time.Time
	limits  map[Channel]Limit
}

// Option customises a Service.
type Option func(*Service)

// WithCounter overrides the default MemoryCounter (tests inject a fake).
func WithCounter(c Counter) Option {
	return func(s *Service) {
		if c != nil {
			s.counter = c
		}
	}
}

// WithNowFunc overrides the service clock (deterministic period-end tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// WithLimits overrides the default limit table.
func WithLimits(m map[Channel]Limit) Option {
	return func(s *Service) {
		if m != nil {
			s.limits = make(map[Channel]Limit, len(m))
			for k, v := range m {
				s.limits[k] = v
			}
		}
	}
}

// defaultLimits mirrors tech plan §4.3.3: WeCom group + 1v1 are capped at
// 1/day, 7/week, monthly=days-in-month. Subscribe/SMS are absent (unlimited).
var defaultLimits = map[Channel]Limit{
	ChannelWeworkGroup: {Daily: 1, Weekly: 7, Monthly: 0},
	ChannelWeworkOneV:  {Daily: 1, Weekly: 7, Monthly: 0},
}

// New constructs a Service with a MemoryCounter, time.Now, and the default
// WeCom limits.
func New(opts ...Option) *Service {
	s := &Service{
		now:    time.Now,
		limits: make(map[Channel]Limit, len(defaultLimits)),
	}
	for k, v := range defaultLimits {
		s.limits[k] = v
	}
	for _, opt := range opts {
		opt(s)
	}
	// Create the default MemoryCounter AFTER options are applied so it shares
	// the service's injected clock (tests advance a shared clock variable and
	// expect the counter's lazy-expiry to see the same time).
	if s.counter == nil {
		s.counter = NewMemoryCounter(WithMemoryNowFunc(s.now))
	}
	return s
}
