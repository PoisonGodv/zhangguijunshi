package riskwindow

// Service.RecordBlock / RecordSend / RecentEvents implement the sliding-window
// risk monitor (BE-B-10, CONTRACT §11.3.4). Each method resolves the tenant from
// ctx, then under the service mutex appends a timestamp to the tenant's bucket
// list, prunes entries older than the window, and compares the surviving count
// to the threshold. A threshold crossing appends a RiskEvent (also pruned by
// window). The methods NEVER block the caller: they return (alerted, event,
// nil) even when alerting — only a missing tenant yields an error.

import (
	"context"
	"fmt"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// tenantState is the per-tenant rolling state. The bucket slices hold the
// timestamp of each block/send; their length after pruning == the in-window
// observed count. events holds the RiskEvents recorded for this tenant.
type tenantState struct {
	blockBuckets []time.Time
	sendBuckets  []time.Time
	events       []*RiskEvent
}

// state returns the tenant's state, allocating it on first use. The caller MUST
// hold s.mu.
func (s *Service) state(tid int64) *tenantState {
	st := s.states[tid]
	if st == nil {
		st = &tenantState{}
		s.states[tid] = st
	}
	return st
}

// thresholdFor returns the configured threshold for one event type.
func (s *Service) thresholdFor(typ EventType) int64 {
	switch typ {
	case EventHighBlockRate:
		return s.blockThreshold
	case EventBurstSend:
		return s.sendThreshold
	}
	return 0
}

// formatWindow renders the window as a concise label for RiskEvent.Window
// (CONTRACT §11.3.4 example "1h"). time.Duration.String() would yield
// "1h0m0s" for a whole hour, which obscures the label, so whole-hour and
// whole-minute windows are collapsed to "Nh" / "Nm".
func formatWindow(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return d.String()
}

// appendEvent stamps a new RiskEvent, prunes the tenant's event list by the
// window, and returns a defensive copy. The caller MUST hold s.mu.
func (s *Service) appendEvent(st *tenantState, tid int64, typ EventType, observed int64, now time.Time) *RiskEvent {
	s.eventSeq++
	ev := &RiskEvent{
		ID:        s.eventSeq,
		TenantID:  tid,
		Type:      typ,
		Window:    formatWindow(s.window),
		Threshold: s.thresholdFor(typ),
		Observed:  observed,
		CreatedAt: now,
	}
	st.events = append(st.events, ev)
	st.events = pruneTimesEvents(st.events, now.Add(-s.window))
	return cloneEvent(ev)
}

// pruneTimes drops bucket entries older than the window (an entry exactly
// window-old counts as expired; only strictly-after-cutoff survives). It
// compacts in place, preserving insertion order (oldest first). The backing
// array is reused, which is safe because time.Time is a value type.
func pruneTimes(ts []time.Time, cutoff time.Time) []time.Time {
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// pruneTimesEvents is the RiskEvent analogue of pruneTimes.
func pruneTimesEvents(ev []*RiskEvent, cutoff time.Time) []*RiskEvent {
	kept := ev[:0]
	for _, e := range ev {
		if e.CreatedAt.After(cutoff) {
			kept = append(kept, e)
		}
	}
	return kept
}

// cloneEvent returns a defensive copy of e (nil-safe).
func cloneEvent(e *RiskEvent) *RiskEvent {
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
}

// RecordBlock records one blocked reach attempt for the ctx tenant and reports
// whether the rolling block count crossed BlockThreshold. It ALWAYS returns a
// nil error on the happy path: a soft rail never rejects the caller, so the
// reach layer can keep recording after an alert. The reason is reserved for
// future structured logging and does not affect counting.
func (s *Service) RecordBlock(ctx context.Context, reason string) (alerted bool, event *RiskEvent, err error) {
	tid, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return false, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	st := s.state(tid)
	st.blockBuckets = append(st.blockBuckets, now)
	st.blockBuckets = pruneTimes(st.blockBuckets, now.Add(-s.window))
	observed := int64(len(st.blockBuckets))
	if observed > s.blockThreshold {
		ev := s.appendEvent(st, tid, EventHighBlockRate, observed, now)
		return true, ev, nil
	}
	return false, nil, nil
}

// RecordSend records one outbound send for the ctx tenant and reports whether
// the rolling send count crossed SendThreshold. Like RecordBlock it never
// rejects the caller.
func (s *Service) RecordSend(ctx context.Context) (alerted bool, event *RiskEvent, err error) {
	tid, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return false, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	st := s.state(tid)
	st.sendBuckets = append(st.sendBuckets, now)
	st.sendBuckets = pruneTimes(st.sendBuckets, now.Add(-s.window))
	observed := int64(len(st.sendBuckets))
	if observed > s.sendThreshold {
		ev := s.appendEvent(st, tid, EventBurstSend, observed, now)
		return true, ev, nil
	}
	return false, nil, nil
}

// RecentEvents returns the tenant's still-in-window RiskEvents as defensive
// copies, oldest first. Intended for observation / tests; it also prunes the
// tenant's event list as a side effect so an idle tenant's old events are
// reaped on read.
func (s *Service) RecentEvents(ctx context.Context) ([]*RiskEvent, error) {
	tid, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	st := s.state(tid)
	st.events = pruneTimesEvents(st.events, now.Add(-s.window))
	out := make([]*RiskEvent, 0, len(st.events))
	for _, e := range st.events {
		out = append(out, cloneEvent(e))
	}
	return out, nil
}
