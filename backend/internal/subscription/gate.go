package subscription

import "context"

// FeatureGate is the plan paywall. Business code (handlers/middleware) injects
// it to check whether the ctx tenant's current plan permits a feature; it is
// pure logic over a SubscriptionStore (CONTRACT §15.3.2). The plan is read
// live from the store, so a just-upgraded tenant is honored without restart.
//
// disabled carries the MVP "全免费" switch (CONTRACT §25.2, 计划书 §4.2.5 付费墙
// 不启用): when true, Can/Require pass EVERY feature through regardless of plan
// (free L1 all-open). V1 constructs the gate with disabled=false (real billing)
// to enforce the plan matrix again.
type FeatureGate struct {
	store    SubscriptionStore
	disabled bool
}

// NewFeatureGate constructs a FeatureGate over store (paywall enforced).
func NewFeatureGate(store SubscriptionStore) *FeatureGate {
	return &FeatureGate{store: store}
}

// NewDisabledFeatureGate constructs a FeatureGate that passes every feature
// through (MVP 全免费, CONTRACT §25.2). The store is still required so V1 can
// flip back to enforcement without re-plumbing.
func NewDisabledFeatureGate(store SubscriptionStore) *FeatureGate {
	return &FeatureGate{store: store, disabled: true}
}

// Can reports whether the ctx tenant's current plan permits feature. When the
// gate is disabled (MVP 全免费) it returns (true, nil) for every feature. An
// unknown feature returns (false, nil); a tenant/store read failure returns
// (false, err). Comparison is PlanLevel(current) >= featureMatrix[feature].
func (g *FeatureGate) Can(ctx context.Context, feature string) (bool, error) {
	if g.disabled {
		return true, nil // MVP 全免费 (§25.2): paywall off, everything allowed
	}
	required, known := featureMatrix[feature]
	if !known {
		return false, nil
	}
	tenantID, err := tenantIDFromCtx(ctx)
	if err != nil {
		return false, err
	}
	plan, err := g.store.CurrentPlan(ctx, tenantID)
	if err != nil {
		return false, err
	}
	return PlanLevel(plan) >= required, nil
}

// Require checks the feature, returning ErrPlanUpgradeRequired when the ctx
// tenant's plan does not permit it. Underlying failures (no tenant, store
// error) are propagated as-is rather than masked as an upgrade prompt.
func (g *FeatureGate) Require(ctx context.Context, feature string) error {
	ok, err := g.Can(ctx, feature)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPlanUpgradeRequired
	}
	return nil
}
