package riskwindow

import (
	"context"
	"errors"
	"sync"
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

func TestRecordBlock_DefaultThreshold_NoAlertUntilExceeded(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	// Default BlockThreshold = 10; observed must STRICTLY exceed it.
	for i := 0; i < 10; i++ {
		alerted, ev, err := s.RecordBlock(ctx, "freq_circuit")
		if err != nil {
			t.Fatalf("call %d: unexpected err %v", i, err)
		}
		if alerted || ev != nil {
			t.Fatalf("call %d (observed=%d): should not alert yet", i, i+1)
		}
	}
	// 11th block: observed=11 > 10 → alert.
	alerted, ev, err := s.RecordBlock(ctx, "content_filter")
	if err != nil {
		t.Fatalf("11th: unexpected err %v", err)
	}
	if !alerted || ev == nil {
		t.Fatalf("11th block should alert: alerted=%v ev=%v", alerted, ev)
	}
	if ev.Type != EventHighBlockRate {
		t.Errorf("event type = %q, want %q", ev.Type, EventHighBlockRate)
	}
	if ev.TenantID != 1001 {
		t.Errorf("event tenant = %d, want 1001", ev.TenantID)
	}
	if ev.Threshold != 10 {
		t.Errorf("event threshold = %d, want 10", ev.Threshold)
	}
	if ev.Observed != 11 {
		t.Errorf("event observed = %d, want 11", ev.Observed)
	}
	if ev.Window != "1h" {
		t.Errorf("event window = %q, want %q", ev.Window, "1h")
	}
	if !ev.CreatedAt.Equal(clock) {
		t.Errorf("event created_at = %v, want %v", ev.CreatedAt, clock)
	}
	if ev.ID <= 0 {
		t.Errorf("event id = %d, want > 0", ev.ID)
	}
}

func TestRecordBlock_AlertDoesNotBlock(t *testing.T) {
	// A SOFT rail: even after the threshold is crossed, subsequent calls keep
	// returning nil error and keep recording (and alerting).
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithBlockThreshold(2), WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	// 3 blocks: observed 1,2,3 → alert on 3rd (3 > 2).
	alerted, _, err := s.RecordBlock(ctx, "x")
	if err != nil || alerted {
		t.Fatalf("1st should not alert: %v %v", alerted, err)
	}
	alerted, _, err = s.RecordBlock(ctx, "x")
	if err != nil || alerted {
		t.Fatalf("2nd should not alert (2 not > 2): %v %v", alerted, err)
	}
	alerted, ev, err := s.RecordBlock(ctx, "x")
	if err != nil || !alerted || ev == nil {
		t.Fatalf("3rd should alert and never error: %v %v %v", alerted, ev, err)
	}
	// 4th: still no error, still alerts (observed 4 > 2). The rail did not block.
	alerted, ev, err = s.RecordBlock(ctx, "x")
	if err != nil {
		t.Fatalf("4th must not error after alert: %v", err)
	}
	if !alerted || ev == nil {
		t.Fatalf("4th should still alert: %v %v", alerted, ev)
	}

	evs, err := s.RecentEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("expect 2 events recorded (3rd + 4th), got %d", len(evs))
	}
}

func TestRecordSend_BurstThreshold(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	// Default SendThreshold = 200; the 201st send trips burst_send.
	for i := 0; i < 200; i++ {
		alerted, ev, err := s.RecordSend(ctx)
		if err != nil {
			t.Fatalf("send %d: unexpected err %v", i, err)
		}
		if alerted || ev != nil {
			t.Fatalf("send %d should not alert", i)
		}
	}
	alerted, ev, err := s.RecordSend(ctx)
	if err != nil || !alerted || ev == nil {
		t.Fatalf("201st send should alert burst_send: %v %v %v", alerted, ev, err)
	}
	if ev.Type != EventBurstSend {
		t.Errorf("event type = %q, want %q", ev.Type, EventBurstSend)
	}
	if ev.Threshold != 200 || ev.Observed != 201 {
		t.Errorf("event thresh/observed = %d/%d, want 200/201", ev.Threshold, ev.Observed)
	}
}

func TestSlidingWindow_PruneExpiredBuckets(t *testing.T) {
	// Window = 1 minute. Record a burst at t0, then advance past the window:
	// the old buckets must stop counting and the old event must be reaped.
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithWindow(time.Minute), WithBlockThreshold(2), WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	// 3 blocks at t0 → 1 event (observed=3 > 2).
	for i := 0; i < 3; i++ {
		_, _, err := s.RecordBlock(ctx, "freq_circuit")
		if err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.RecentEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 {
		t.Fatalf("expect 1 event before advance, got %d", len(evs))
	}

	// Advance 2 minutes: everything older than 1m is expired.
	clock = clock.Add(2 * time.Minute)

	// One fresh block: old 3 pruned, observed=1, must NOT alert.
	alerted, ev, err := s.RecordBlock(ctx, "freq_circuit")
	if err != nil {
		t.Fatal(err)
	}
	if alerted || ev != nil {
		t.Fatalf("after prune a single block must not alert: %v %v", alerted, ev)
	}

	// The old event must have been reaped (its CreatedAt=t0 < now-window).
	evs, err = s.RecentEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("expect old event pruned after advance, got %d events", len(evs))
	}
}

func TestSlidingWindow_StillInWindowCounts(t *testing.T) {
	// Buckets younger than the window survive: 2 blocks at t0 (no alert), then
	// advance 30s (< 1m window) and the 3rd block sees observed=3 → alert.
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithWindow(time.Minute), WithBlockThreshold(2), WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	for i := 0; i < 2; i++ {
		_, _, _ = s.RecordBlock(ctx, "x")
	}
	clock = clock.Add(30 * time.Second)
	alerted, ev, err := s.RecordBlock(ctx, "x")
	if err != nil {
		t.Fatal(err)
	}
	if !alerted || ev == nil {
		t.Fatalf("3rd block within window should alert (observed=3): %v %v", alerted, ev)
	}
	if ev.Observed != 3 {
		t.Errorf("observed = %d, want 3 (old buckets retained)", ev.Observed)
	}
}

func TestRecordBlock_ReasonDoesNotAffectCounting(t *testing.T) {
	// Different reason strings must not change the observed count.
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithBlockThreshold(2), WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	for _, r := range []string{"freq_circuit", "content_filter", "unsubscribed"} {
		if _, _, err := s.RecordBlock(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	alerted, ev, err := s.RecordBlock(ctx, "risk")
	if err != nil || !alerted || ev == nil {
		t.Fatalf("4th block should alert regardless of reasons: %v %v %v", alerted, ev, err)
	}
	if ev.Observed != 4 {
		t.Errorf("observed = %d, want 4", ev.Observed)
	}
}

func TestRecentEvents_OnlyInWindow(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithWindow(time.Minute), WithBlockThreshold(1), WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	// 3 blocks with threshold 1: blocks 2 and 3 alert (observed strictly > 1).
	for i := 0; i < 3; i++ {
		if _, _, err := s.RecordBlock(ctx, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if evs, _ := s.RecentEvents(ctx); len(evs) != 2 {
		t.Fatalf("expect 2 events, got %d", len(evs))
	}
	// Advance past window: RecentEvents prunes and returns empty.
	clock = clock.Add(2 * time.Minute)
	evs, err := s.RecentEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("expect events pruned, got %d", len(evs))
	}
}

func TestCrossTenantIsolation(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithBlockThreshold(2), WithNowFunc(func() time.Time { return clock }))
	ctxA := ctxWithTenant(1001)
	ctxB := ctxWithTenant(1002)

	// Tenant A crosses the threshold (3 blocks → 1 event).
	for i := 0; i < 3; i++ {
		if _, _, err := s.RecordBlock(ctxA, "x"); err != nil {
			t.Fatal(err)
		}
	}
	// Tenant B's window is independent: no events, a single block does not alert.
	if evs, _ := s.RecentEvents(ctxB); len(evs) != 0 {
		t.Fatalf("tenant B should have no events, got %d", len(evs))
	}
	alerted, ev, err := s.RecordBlock(ctxB, "x")
	if err != nil {
		t.Fatal(err)
	}
	if alerted || ev != nil {
		t.Fatalf("tenant B single block should not alert: %v %v", alerted, ev)
	}
	// Tenant A's recorded event survives untouched.
	if evs, _ := s.RecentEvents(ctxA); len(evs) != 1 {
		t.Fatalf("tenant A should still have 1 event, got %d", len(evs))
	}
}

func TestNoTenant_Errors(t *testing.T) {
	s := New(WithBlockThreshold(1), WithSendThreshold(1))
	ctx := context.Background()

	alerted, ev, err := s.RecordBlock(ctx, "x")
	if err == nil || alerted || ev != nil {
		t.Fatalf("RecordBlock without tenant must error: err=%v alerted=%v ev=%v", err, alerted, ev)
	}
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Errorf("RecordBlock err = %v, want ErrNoTenant", err)
	}
	alerted, ev, err = s.RecordSend(ctx)
	if err == nil || alerted || ev != nil {
		t.Fatalf("RecordSend without tenant must error: err=%v alerted=%v ev=%v", err, alerted, ev)
	}
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Errorf("RecordSend err = %v, want ErrNoTenant", err)
	}
	evs, err := s.RecentEvents(ctx)
	if err == nil || evs != nil {
		t.Fatalf("RecentEvents without tenant must error: err=%v evs=%v", err, evs)
	}
	if !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Errorf("RecentEvents err = %v, want ErrNoTenant", err)
	}
}

func TestOptions_WindowString(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithWindow(30*time.Second), WithBlockThreshold(1), WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	// 2 blocks with threshold 1: the 2nd (observed=2 > 1) alerts.
	if _, _, err := s.RecordBlock(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	_, ev, err := s.RecordBlock(ctx, "x")
	if err != nil || ev == nil {
		t.Fatalf("2nd block should alert: %v %v", ev, err)
	}
	if ev.Window != "30s" {
		t.Errorf("event window = %q, want %q", ev.Window, "30s")
	}
}

func TestConcurrent_NoRace(t *testing.T) {
	// Exercises the service mutex under -race: many goroutines, both methods,
	// two tenants. No assertions on exact counts — the -race detector is the
	// point; we only require every call to succeed.
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithBlockThreshold(1<<62), WithSendThreshold(1<<62), WithNowFunc(func() time.Time { return clock }))
	ctxA := ctxWithTenant(1001)
	ctxB := ctxWithTenant(1002)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ctx := ctxA
			if id%2 == 0 {
				ctx = ctxB
			}
			for i := 0; i < 50; i++ {
				if _, _, err := s.RecordBlock(ctx, "x"); err != nil {
					t.Errorf("RecordBlock: %v", err)
					return
				}
				if _, _, err := s.RecordSend(ctx); err != nil {
					t.Errorf("RecordSend: %v", err)
					return
				}
				_, _ = s.RecentEvents(ctx)
			}
		}(g)
	}
	wg.Wait()
}
