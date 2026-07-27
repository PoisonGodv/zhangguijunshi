package reach

// MemorySender is the in-memory Sender dev stub: it records every SendPayload it
// receives and returns nil (a dev stand-in for the real WeCom / subscribe / SMS
// adapters, which land in M3). It is concurrency-safe and returns defensive
// copies so tests cannot mutate the recorded history.

import (
	"context"
	"sync"
)

// MemorySender stores received payloads in order.
type MemorySender struct {
	mu       sync.Mutex
	payloads []*SendPayload
}

// NewMemorySender constructs an empty MemorySender.
func NewMemorySender() *MemorySender {
	return &MemorySender{}
}

// clonePayload returns a defensive copy of p.
func clonePayload(p *SendPayload) *SendPayload {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// Send records a defensive copy of p and returns nil (dev stub never fails).
// Tests that need to exercise send-failure inject a custom Sender fake instead.
func (s *MemorySender) Send(_ context.Context, p *SendPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloads = append(s.payloads, clonePayload(p))
	return nil
}

// Payloads returns defensive copies of every recorded payload, in send order.
func (s *MemorySender) Payloads() []*SendPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*SendPayload, len(s.payloads))
	for i, p := range s.payloads {
		out[i] = clonePayload(p)
	}
	return out
}
