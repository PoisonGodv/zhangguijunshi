package freqcircuit

// Service.Allow / Remaining implement the frequency-circuit logic (BE-B-04).
// Allow does a three-period check (daily / weekly / monthly) atomically: it
// increments all three, and if any exceeds its cap it rolls ALL three back so a
// blocked message leaves no phantom count behind. The period-end (TTL) is
// computed from the injected clock: end-of-day, end-of-week (Sunday 23:59:59),
// end-of-month.

import (
	"context"
	"fmt"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// periodOrder is the order periods are checked; deterministic for testability.
var periodOrder = []Period{PeriodDaily, PeriodWeekly, PeriodMonthly}

// limitFor returns the configured Limit for ch. A channel absent from the map
// is unlimited (ok=false).
func (s *Service) limitFor(ch Channel) (Limit, bool) {
	lim, ok := s.limits[ch]
	return lim, ok
}

// monthlyMax resolves the monthly cap: Monthly>0 uses it directly; Monthly=0
// means "days in the current month" (计划书 口径 for WeCom channels).
func (s *Service) monthlyMax(lim Limit) int64 {
	if lim.Monthly > 0 {
		return lim.Monthly
	}
	return int64(daysInMonth(s.now()))
}

// maxFor returns the cap for one period.
func (s *Service) maxFor(lim Limit, p Period) int64 {
	switch p {
	case PeriodDaily:
		return lim.Daily
	case PeriodWeekly:
		return lim.Weekly
	case PeriodMonthly:
		return s.monthlyMax(lim)
	}
	return 0
}

// key builds the Redis-shape counter key: t:{tid}:freq:{ch}:{period}.
func key(tid int64, ch Channel, p Period) string {
	return fmt.Sprintf("t:%d:freq:%s:%s", tid, ch, p)
}

// endOfPeriod returns the TTL expiry for one period relative to t: end of the
// current day (daily), end of the current week — Sunday 23:59:59 (weekly), end
// of the current month (monthly).
func endOfPeriod(p Period, t time.Time) time.Time {
	switch p {
	case PeriodDaily:
		y, m, d := t.Date()
		return time.Date(y, m, d, 23, 59, 59, 999999999, t.Location())
	case PeriodWeekly:
		// Monday=0 ... Sunday=6 (Go weekday: Sunday=0). Days until Sunday.
		wd := int(t.Weekday()) // Sunday=0
		var delta int
		if wd == 0 {
			delta = 0 // already Sunday
		} else {
			delta = 7 - wd
		}
		end := t.AddDate(0, 0, delta)
		y, m, d := end.Date()
		return time.Date(y, m, d, 23, 59, 59, 999999999, t.Location())
	case PeriodMonthly:
		y, m, _ := t.Date()
		firstNext := time.Date(y, m+1, 1, 0, 0, 0, 0, t.Location())
		return firstNext.Add(-time.Nanosecond)
	}
	return t
}

// daysInMonth returns the day count of t's month.
func daysInMonth(t time.Time) int {
	y, m, _ := t.Date()
	// First day of next month, minus one day = last day of this month.
	firstNext := time.Date(y, m+1, 1, 0, 0, 0, 0, t.Location())
	return firstNext.AddDate(0, 0, -1).Day()
}

// Allow checks whether one more message on ch is permitted for the ctx tenant.
// It increments all three period counters, and if any exceeds its cap it
// decrements all three back (so a blocked attempt leaves no count) and returns
// (false, "freq_<period>"). A channel with no configured limit is unlimited.
func (s *Service) Allow(ctx context.Context, ch Channel) (allowed bool, reason string, err error) {
	tid, err := multitenancy.TenantID(ctx)
	if err != nil {
		return false, "", err
	}
	lim, ok := s.limitFor(ch)
	if !ok {
		return true, "", nil // 无配置 = 不限
	}

	now := s.now()
	type inc struct {
		key string
		val int64
	}
	var incs []inc
	for _, p := range periodOrder {
		k := key(tid, ch, p)
		n, e := s.counter.Incr(ctx, k, endOfPeriod(p, now))
		if e != nil {
			// 回滚已 Incr 的
			for _, x := range incs {
				_ = s.counter.Decr(ctx, x.key)
			}
			return false, "", e
		}
		incs = append(incs, inc{key: k, val: n})
	}

	// 任一档超限 → 全部回退
	for i, p := range periodOrder {
		max := s.maxFor(lim, p)
		if max <= 0 {
			continue // 该档不限(如 monthly=0 但 monthlyMax 已算为天数, 不会为 0; 防御)
		}
		if incs[i].val > max {
			for _, x := range incs {
				_ = s.counter.Decr(ctx, x.key)
			}
			return false, "freq_" + string(p), nil
		}
	}
	return true, "", nil
}

// Remaining returns the residual quotas for ch on the ctx tenant (daily /
// weekly / monthly). A channel with no configured limit returns -1 for all
// three (unlimited). Values are clamped at 0 (never negative).
func (s *Service) Remaining(ctx context.Context, ch Channel) (daily, weekly, monthly int64, err error) {
	tid, err := multitenancy.TenantID(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	lim, ok := s.limitFor(ch)
	if !ok {
		return -1, -1, -1, nil // 不限
	}
	get := func(p Period) (int64, error) {
		n, e := s.counter.Get(ctx, key(tid, ch, p))
		if e != nil {
			return 0, e
		}
		return n, nil
	}
	dn, e := get(PeriodDaily)
	if e != nil {
		return 0, 0, 0, e
	}
	wn, e := get(PeriodWeekly)
	if e != nil {
		return 0, 0, 0, e
	}
	mn, e := get(PeriodMonthly)
	if e != nil {
		return 0, 0, 0, e
	}
	daily = lim.Daily - dn
	weekly = lim.Weekly - wn
	monthly = s.monthlyMax(lim) - mn
	if daily < 0 {
		daily = 0
	}
	if weekly < 0 {
		weekly = 0
	}
	if monthly < 0 {
		monthly = 0
	}
	return daily, weekly, monthly, nil
}
