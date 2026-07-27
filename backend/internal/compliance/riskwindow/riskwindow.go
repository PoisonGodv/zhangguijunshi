// Package riskwindow implements the tenant-level sliding-window risk monitor
// (BE-B-10, CONTRACT §11.3.4 / tech plan §4.3.6). It observes reach blocks and
// sends over a rolling window (default 1h) and, when a tenant crosses a
// threshold (too many blocks ⇒ high_block_rate, too many sends ⇒ burst_send),
// records a RiskEvent for downstream alerting.
//
// This is a SOFT rail: RecordBlock / RecordSend NEVER reject the caller — they
// always return (alerted, event, nil) on the happy path. Real alert delivery
// (飞书 / SMS) is deferred; here we only persist the event in-memory. The
// sliding window is a "bucket list pruned by the injected clock" implementation
// (no Redis dependency), so it is fully deterministic in unit tests: advance
// the clock past the window and the old buckets/events stop counting.
//
// Tenant is resolved from ctx via multitenancy.EnforceTenant on every call; the
// package has zero domain dependencies (it imports only pkg/multitenancy).
package riskwindow

import (
	"sync"
	"time"
)

// EventType labels a risk signal.
type EventType string

const (
	// EventHighBlockRate fires when the count of blocked reach attempts in the
	// window exceeds BlockThreshold (a proxy for a deteriorating block rate).
	EventHighBlockRate EventType = "high_block_rate"
	// EventBurstSend fires when the absolute send count in the window exceeds
	// SendThreshold (a sudden burst of outbound messages).
	EventBurstSend EventType = "burst_send"
)

// RiskEvent is one recorded threshold crossing. Window is the human-readable
// window size (e.g. "1h"); Observed is the count that tripped Threshold.
type RiskEvent struct {
	ID, TenantID int64
	Type         EventType
	Window       string
	Threshold    int64
	Observed     int64
	CreatedAt    time.Time
}

// Default window / thresholds (CONTRACT §11.3.4).
const (
	defaultWindow         = time.Hour
	defaultBlockThreshold = int64(10)
	defaultSendThreshold  = int64(200)
)

// Service is the sliding-window risk monitor. Rolling state is keyed by tenant
// id so one tenant's burst never influences another's window. All access is
// serialised under a single mutex; the clock is injected for deterministic
// pruning tests. State is built-in (no Repo interface): the window is small,
// short-lived, and observed in-memory — a future Redis/pgx backing would wrap
// the same method signatures.
type Service struct {
	mu             sync.Mutex
	states         map[int64]*tenantState
	window         time.Duration
	blockThreshold int64
	sendThreshold  int64
	now            func() time.Time
	eventSeq       int64
}

// Option customises a Service.
type Option func(*Service)

// WithWindow overrides the rolling window (default 1h). A non-positive value is
// ignored so a misconfigured caller cannot shrink the window to zero.
func WithWindow(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.window = d
		}
	}
}

// WithBlockThreshold overrides the high_block_rate threshold (default 10). A
// non-positive value is ignored.
func WithBlockThreshold(n int64) Option {
	return func(s *Service) {
		if n > 0 {
			s.blockThreshold = n
		}
	}
}

// WithSendThreshold overrides the burst_send threshold (default 200). A
// non-positive value is ignored.
func WithSendThreshold(n int64) Option {
	return func(s *Service) {
		if n > 0 {
			s.sendThreshold = n
		}
	}
}

// WithNowFunc overrides the service clock (deterministic window-prune tests).
// A nil function is ignored.
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// New constructs a riskwindow Service with the default window / thresholds and
// time.Now.
func New(opts ...Option) *Service {
	s := &Service{
		states:         make(map[int64]*tenantState),
		window:         defaultWindow,
		blockThreshold: defaultBlockThreshold,
		sendThreshold:  defaultSendThreshold,
		now:            time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}
