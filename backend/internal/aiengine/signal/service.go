package signal

import (
	"time"
)

// Service implements Collector. It pulls inventory/members/txn via the Lookuper
// port (best-effort, concurrent) and mocks weather/holiday in-memory. Collect
// never returns a nil Bundle.
type Service struct {
	businessType string
	city         string
	lk           Lookuper
	now          func() time.Time
	weatherFn    func() Weather
	holidayFn    func() Holiday
}

// Option configures a Service.
type Option func(*Service)

// WithNowFunc injects a deterministic clock (tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) { s.now = f }
}

// WithWeatherMock overrides the in-memory weather mock.
func WithWeatherMock(fn func() Weather) Option {
	return func(s *Service) { s.weatherFn = fn }
}

// WithHolidayMock overrides the in-memory holiday mock.
func WithHolidayMock(fn func() Holiday) Option {
	return func(s *Service) { s.holidayFn = fn }
}

// NewCollector builds a Collector. businessType defaults to "fruit" (cold-start
// sample business per CONTRACT §12.3.8) when empty. city is carried through to
// the Bundle. lk may be nil — Collect degrades to a zero Bundle in that case.
func NewCollector(businessType, city string, lk Lookuper, opts ...Option) *Service {
	s := &Service{
		businessType: businessType,
		city:         city,
		lk:           lk,
		now:          time.Now,
	}
	if s.businessType == "" {
		s.businessType = "fruit"
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// mockWeather returns the in-memory weather signal (fruit default "normal").
func (s *Service) mockWeather() Weather {
	if s.weatherFn != nil {
		return s.weatherFn()
	}
	return Weather{Condition: "normal", TempC: 20.0, Forecast: "晴"}
}

// mockHoliday returns the in-memory holiday signal (default 无).
func (s *Service) mockHoliday() Holiday {
	if s.holidayFn != nil {
		return s.holidayFn()
	}
	return Holiday{Name: "无", DaysTo: -1, IsFestival: false}
}

// computeDataQuality derives a 0-100 trace score from what the Lookuper
// returned. All-empty → 0 (cold-start entry); each present path adds signal.
func computeDataQuality(inv []InventorySig, mem MemberStats, txn TxnSig) int {
	score := 0
	if len(inv) > 0 {
		score += 40
	}
	if mem.Total > 0 || mem.Sleep30 > 0 || len(mem.HighValue) > 0 || len(mem.BirthdayToday) > 0 {
		score += 30
	}
	if txn.OrderCount > 0 || txn.RevenueCents > 0 || txn.NewMembers > 0 {
		score += 30
	}
	return score
}
