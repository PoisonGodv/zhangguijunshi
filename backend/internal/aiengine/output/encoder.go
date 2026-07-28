package output

import (
	"context"
	"time"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/compliance/contentfilter"
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// Encoder is the Encode stage port. It persists the Top3 + Bundle as today's
// strategy and returns the Advice. The only error it returns is a persistence
// failure from strategy.Service.SaveAdvice (CONTRACT §12.3.8: "Encode 失败除
// 外→返回 error"); copy filtering / marshal never error.
type Encoder interface {
	Encode(ctx context.Context, tenantID, storeID int64, top3 []*agent.EnhancedItem, bundle *signal.Bundle, degrade string) (*Advice, error)
}

// Service implements Encoder. svc is the strategy orchestrator (which enforces
// tenant isolation); filter is the contentfilter (M2-S2 BE-B-05); now supplies
// the date / created-at clock.
type Service struct {
	svc    *strategy.Service
	filter *contentfilter.Filter
	now    func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithFilter overrides the default contentfilter.New().
func WithFilter(f *contentfilter.Filter) Option {
	return func(s *Service) {
		if f != nil {
			s.filter = f
		}
	}
}

// WithNowFunc overrides the service clock (deterministic date tests).
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) {
		if f != nil {
			s.now = f
		}
	}
}

// NewEncoder builds an Encoder. svc must be non-nil; the default contentfilter
// and wall clock are installed and overridable via options.
func NewEncoder(svc *strategy.Service, opts ...Option) *Service {
	s := &Service{svc: svc, filter: contentfilter.New(), now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}
