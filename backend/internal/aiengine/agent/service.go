package agent

import (
	"context"

	"github.com/zhangguijunshi/backend/internal/aiengine/gateway"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
)

// Service implements Stage. gw may be a no-provider gateway (dev without API
// keys) or even nil — both degrade per-item to templates. tier selects the
// gateway model class (default TierLite per tech plan §7.8.2).
type Service struct {
	gw   *gateway.Gateway
	tier gateway.TaskTier
}

// Option configures a Service.
type Option func(*Service)

// WithTier overrides the gateway task tier (default TierLite).
func WithTier(t gateway.TaskTier) Option {
	return func(s *Service) {
		if t != "" {
			s.tier = t
		}
	}
}

// New builds an Agent Stage. gw may be nil or a no-provider gateway; both cases
// degrade to templates without error.
func New(gw *gateway.Gateway, opts ...Option) *Service {
	s := &Service{gw: gw, tier: gateway.TierLite}
	for _, o := range opts {
		o(s)
	}
	return s
}

// EnhanceOrSkip walks each RoutedItem, attempts one LLM polish call, and falls
// back to a template item on any failure. It never returns nil for a non-empty
// input and never returns an error. See the package doc for the degrade rule.
func (s *Service) EnhanceOrSkip(ctx context.Context, tenantID int64, r *rule.Routed, degrade *string) *Enhanced {
	out := &Enhanced{TenantID: tenantID}
	if r == nil {
		return out
	}
	out.TenantID = r.TenantID
	if out.TenantID == 0 {
		out.TenantID = tenantID
	}

	llmHitAny := false
	firstReason := "" // first per-item failure reason (used only if no LLM hit at all)

	for i := range r.Items {
		ri := r.Items[i]
		ei, hit, reason := s.enhanceOne(ctx, tenantID, ri)
		out.Items = append(out.Items, ei)
		if hit {
			llmHitAny = true
		} else if firstReason == "" {
			firstReason = reason
		}
	}

	// Write-once: only record the LLM-failure degrade when the whole call never
	// hit the LLM AND no upstream stage already claimed a reason (cold_start).
	if !llmHitAny && firstReason != "" && degrade != nil && *degrade == "" {
		*degrade = firstReason
	}
	return out
}
