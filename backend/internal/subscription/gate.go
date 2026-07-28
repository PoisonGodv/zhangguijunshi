package subscription

import "context"

// FeatureGate is the plan paywall. Business code (handlers/middleware) injects
// it to check whether the ctx tenant's current plan permits a feature; it is
// pure logic over a SubscriptionStore (CONTRACT §15.3.2). The plan is read
// live from the store, so a just-upgraded tenant is honored without restart.
type FeatureGate struct{ store SubscriptionStore }

// NewFeatureGate constructs a FeatureGate over store.
func NewFeatureGate(store SubscriptionStore) *FeatureGate {
	return &FeatureGate{store: store}
}

// Can reports whether the ctx tenant's current plan permits feature. An
// unknown feature returns (false, nil); a tenant/store read failure returns
// (false, err). Comparison is PlanLevel(current) >= featureMatrix[feature].
func (g *FeatureGate) Can(ctx context.Context, feature string) (bool, error) {
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
