package freqcircuit

import (
	"context"
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

func TestAllow_UnlimitedChannel_AlwaysTrue(t *testing.T) {
	s := New(WithNowFunc(func() time.Time { return time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC) }))
	ctx := ctxWithTenant(1001)
	// SubscribeMsg / SMS absent from default limits → unlimited
	for i := 0; i < 100; i++ {
		ok, _, err := s.Allow(ctx, ChannelSubscribeMsg)
		if err != nil || !ok {
			t.Fatalf("unlimited channel should always allow (call %d): ok=%v err=%v", i, ok, err)
		}
	}
}

func TestAllow_DailyLimit_OnePerDay(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	ok, reason, err := s.Allow(ctx, ChannelWeworkGroup)
	if err != nil || !ok || reason != "" {
		t.Fatalf("first daily call should allow: ok=%v reason=%q err=%v", ok, reason, err)
	}
	ok, reason, err = s.Allow(ctx, ChannelWeworkGroup)
	if err != nil || ok || reason != "freq_daily" {
		t.Fatalf("second daily call should block freq_daily: ok=%v reason=%q err=%v", ok, reason, err)
	}
	// After a blocked call the count must roll back: Remaining daily should be 0.
	d, w, m, err := s.Remaining(ctx, ChannelWeworkGroup)
	if err != nil {
		t.Fatal(err)
	}
	if d != 0 || w != 6 || m != 30 {
		t.Fatalf("after 1 send + 1 blocked: expect daily=0 weekly=6 monthly=30, got d=%d w=%d m=%d", d, w, m)
	}
}

// TestAllow_RollbackLeavesNoPhantomCount verifies that a blocked Allow rolls
// back ALL three period counters (not just the one that tripped). Uses a shared
// mutable clock so the Service and its MemoryCounter advance together.
func TestAllow_RollbackLeavesNoPhantomCount(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC) // Wednesday
	s := New(WithNowFunc(func() time.Time { return clock }))
	ctx := ctxWithTenant(1001)

	_, _, _ = s.Allow(ctx, ChannelWeworkGroup) // 1st: ok (d=1,w=1,m=1)
	_, _, _ = s.Allow(ctx, ChannelWeworkGroup) // 2nd: blocked daily, rollback (still 1,1,1)

	// Advance to next day: daily resets, weekly/monthly persist (same week/month)
	clock = clock.Add(26 * time.Hour) // Thursday July 16 12:00
	d, w, m, err := s.Remaining(ctx, ChannelWeworkGroup)
	if err != nil {
		t.Fatal(err)
	}
	// daily: new day → 0 count → 1-0=1; weekly: same week → 1 count → 7-1=6; monthly: same month → 1 count → 31-1=30
	if d != 1 || w != 6 || m != 30 {
		t.Fatalf("rollback check: expect d=1 w=6 m=30, got d=%d w=%d m=%d", d, w, m)
	}
}

func TestAllow_WeeklyLimit(t *testing.T) {
	// Custom limits: daily=10 (won't trigger), weekly=2, monthly=99
	s := New(WithLimits(map[Channel]Limit{
		ChannelWeworkGroup: {Daily: 10, Weekly: 2, Monthly: 99},
	}), WithNowFunc(func() time.Time { return time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC) }))
	ctx := ctxWithTenant(1001)

	for i := 0; i < 2; i++ {
		ok, _, err := s.Allow(ctx, ChannelWeworkGroup)
		if err != nil || !ok {
			t.Fatalf("call %d should allow", i)
		}
	}
	ok, reason, err := s.Allow(ctx, ChannelWeworkGroup)
	if ok || reason != "freq_weekly" {
		t.Fatalf("3rd call should block freq_weekly: ok=%v reason=%q err=%v", ok, reason, err)
	}
}

func TestAllow_MonthlyEqualsDaysInMonth(t *testing.T) {
	// February 2026 has 28 days. Monthly=0 → 28. Daily/weekly high so they
	// don't trigger. Single service with the Feb clock so the MemoryCounter's
	// lazy-expiry shares the same time.
	feb1 := time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)
	s := New(WithLimits(map[Channel]Limit{
		ChannelWeworkGroup: {Daily: 100, Weekly: 100, Monthly: 0},
	}), WithNowFunc(func() time.Time { return feb1 }))
	ctx := ctxWithTenant(1001)

	for i := 0; i < 28; i++ {
		ok, _, err := s.Allow(ctx, ChannelWeworkGroup)
		if err != nil || !ok {
			t.Fatalf("call %d in Feb should allow (monthly=28)", i)
		}
	}
	ok, reason, err := s.Allow(ctx, ChannelWeworkGroup)
	if ok || reason != "freq_monthly" {
		t.Fatalf("29th call should block freq_monthly: ok=%v reason=%q err=%v", ok, reason, err)
	}
}

func TestRemaining_UnlimitedChannel(t *testing.T) {
	s := New(WithNowFunc(func() time.Time { return time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC) }))
	ctx := ctxWithTenant(1001)
	d, w, m, err := s.Remaining(ctx, ChannelSubscribeMsg)
	if err != nil {
		t.Fatal(err)
	}
	if d != -1 || w != -1 || m != -1 {
		t.Fatalf("unlimited channel should return -1,-1,-1, got %d,%d,%d", d, w, m)
	}
}

func TestAllow_CrossTenantIsolation(t *testing.T) {
	clock := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	s := New(WithNowFunc(func() time.Time { return clock }))

	ctxA := ctxWithTenant(1001)
	ctxB := ctxWithTenant(1002)

	ok, _, err := s.Allow(ctxA, ChannelWeworkGroup)
	if err != nil || !ok {
		t.Fatalf("tenant A first should allow")
	}
	ok, _, _ = s.Allow(ctxA, ChannelWeworkGroup)
	if ok {
		t.Fatal("tenant A second should block")
	}
	ok, _, err = s.Allow(ctxB, ChannelWeworkGroup)
	if err != nil || !ok {
		t.Fatalf("tenant B first should allow (isolated): ok=%v err=%v", ok, err)
	}
}

func TestAllow_NoTenant_Errors(t *testing.T) {
	s := New(WithNowFunc(func() time.Time { return time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC) }))
	ok, _, err := s.Allow(context.Background(), ChannelWeworkGroup)
	if err == nil {
		t.Fatal("expect ErrNoTenant without tenant in ctx")
	}
	if ok {
		t.Fatal("should not allow without tenant")
	}
}

func TestDaysInMonth(t *testing.T) {
	cases := []struct {
		t    time.Time
		days int
	}{
		{time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), 31},
		{time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), 28},
		{time.Date(2024, 2, 15, 0, 0, 0, 0, time.UTC), 29},
		{time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC), 30},
		{time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC), 31},
	}
	for _, c := range cases {
		if got := daysInMonth(c.t); got != c.days {
			t.Errorf("daysInMonth(%v) = %d, want %d", c.t, got, c.days)
		}
	}
}

func TestEndOfPeriod(t *testing.T) {
	t0 := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC) // Wednesday
	d := endOfPeriod(PeriodDaily, t0)
	if d.Day() != 15 || d.Hour() != 23 {
		t.Errorf("daily end wrong: %v", d)
	}
	w := endOfPeriod(PeriodWeekly, t0)
	if w.Day() != 19 || w.Hour() != 23 { // Sunday July 19
		t.Errorf("weekly end wrong: %v (want day 19)", w)
	}
	m := endOfPeriod(PeriodMonthly, t0)
	if m.Day() != 31 || m.Month() != time.July {
		t.Errorf("monthly end wrong: %v", m)
	}
}
