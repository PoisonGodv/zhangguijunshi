package subscription

import (
	"context"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
)

// Option configures a Service.
type Option func(*Service)

// WithNowFunc injects a deterministic clock (tests). Defaults to time.Now.
func WithNowFunc(f func() time.Time) Option {
	return func(s *Service) { s.now = f }
}

// Service is the subscription manager: plan state (CurrentPlan/GetSubscription/
// Subscribe) plus the paid-activation port ConfirmPayment (wired from a payment
// Fulfiller). It owns a FeatureGate (exposed via Gate) so handlers can paywall
// features against the ctx tenant's live plan.
type Service struct {
	store SubscriptionStore
	gate  *FeatureGate
	now   func() time.Time
}

// NewService constructs a Service over store, applying opts.
func NewService(store SubscriptionStore, opts ...Option) *Service {
	s := &Service{
		store: store,
		now:   time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.gate = NewFeatureGate(store)
	return s
}

// CurrentPlan returns the ctx tenant's current plan.
func (s *Service) CurrentPlan(ctx context.Context) (string, error) {
	tenantID, err := tenantIDFromCtx(ctx)
	if err != nil {
		return PlanFree, err
	}
	return s.store.CurrentPlan(ctx, tenantID)
}

// GetSubscription returns the ctx tenant's current subscription record (nil if
// none).
func (s *Service) GetSubscription(ctx context.Context) (*Subscription, error) {
	tenantID, err := tenantIDFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	return s.store.Get(ctx, tenantID)
}

// Gate exposes the FeatureGate for handler/middleware paywall checks.
func (s *Service) Gate() *FeatureGate { return s.gate }

// WithGate swaps the FeatureGate (bootstrap wires the disabled MVP gate when
// subscription.enabled=false, CONTRACT §25.2). Additive; the default NewService
// gate (enforced) is unchanged for V1.
func (s *Service) WithGate(g *FeatureGate) { s.gate = g }

// Store exposes the underlying SubscriptionStore (bootstrap needs it to build
// the disabled FeatureGate over the same store, §25.2).
func (s *Service) Store() SubscriptionStore { return s.store }

// ExpireDue transitions every active subscription past its period end to
// expired (CONTRACT §15.8), returning the count affected. It is a GLOBAL batch
// operation run by the cron binary — unlike the per-tenant methods it does NOT
// resolve/enforce a tenant from ctx. Uses the service clock (overridable via
// WithNowFunc for deterministic tests).
func (s *Service) ExpireDue(ctx context.Context) (int, error) {
	return s.store.ExpireDue(ctx, s.now())
}

// Subscribe switches/renews the ctx tenant's plan (manual/admin). For paid
// plans PeriodMonths<=0 defaults to 1 month; free is permanent
// (PeriodEndsAt=nil). Prior active rows are cancelled by the store.
func (s *Service) Subscribe(ctx context.Context, in SubscribeInput) (*Subscription, error) {
	tenantID, err := tenantIDFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	plan := in.Plan
	if plan == "" {
		plan = PlanFree
	}
	source := in.Source
	if source == "" {
		source = "manual"
	}
	sub := &Subscription{
		TenantID:        tenantID,
		Plan:            plan,
		PeriodStartedAt: now,
		PeriodEndsAt:    periodEnds(plan, in.PeriodMonths, now),
		Status:          "active",
		Source:          source,
		PaymentRef:      in.PaymentRef,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	return s.store.Upsert(ctx, sub)
}

// ConfirmPayment activates a subscription from a successful payment (Fulfiller
// adapter port). amountCents is mapped to a plan via PlanPriceCents; a
// non-matching amount returns errcode.ErrParam. The paid plan is monthly
// (1 month from now); source is the payment provider.
func (s *Service) ConfirmPayment(ctx context.Context, tenantID int64, amountCents int64, provider, paymentRef string) (*Subscription, error) {
	plan := planByPrice(amountCents)
	if plan == "" {
		return nil, errcode.ErrParam
	}
	now := s.now()
	end := now.AddDate(0, 1, 0)
	sub := &Subscription{
		TenantID:        tenantID,
		Plan:            plan,
		PeriodStartedAt: now,
		PeriodEndsAt:    &end,
		Status:          "active",
		Source:          provider,
		PaymentRef:      paymentRef,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	return s.store.Upsert(ctx, sub)
}

// planByPrice maps an amount (fen) to a plan, or "" if it matches no
// PlanPriceCents (free has no paid price → never matched here).
func planByPrice(amountCents int64) string {
	switch amountCents {
	case PlanPriceCents(PlanPro):
		return PlanPro
	case PlanPriceCents(PlanFlagship):
		return PlanFlagship
	}
	return ""
}

// periodEnds computes PeriodEndsAt: free→nil (permanent); months>0→now+months;
// otherwise pro/flagship default to 1 month.
func periodEnds(plan string, months int, now time.Time) *time.Time {
	if plan == PlanFree {
		return nil
	}
	if months > 0 {
		t := now.AddDate(0, months, 0)
		return &t
	}
	t := now.AddDate(0, 1, 0)
	return &t
}
