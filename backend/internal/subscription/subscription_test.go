package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// fixedNow is the deterministic clock for tests (no time.Now on hot paths).
var fixedNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// ctxOf builds a context carrying tenantID (merchant source).
func ctxOf(tenantID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		Source:   multitenancy.SourceMerchant,
	})
}

// seedPlan stores an active subscription for tenantID at plan via MemoryStore
// (gate-matrix setup helper).
func seedPlan(store *MemoryStore, tenantID int64, plan string) {
	_, err := store.Upsert(context.Background(), &Subscription{
		TenantID:        tenantID,
		Plan:            plan,
		PeriodStartedAt: fixedNow,
		Status:          "active",
		Source:          "manual",
		CreatedAt:       fixedNow,
		UpdatedAt:       fixedNow,
	})
	if err != nil {
		panic(err)
	}
}

func TestPlanLevel(t *testing.T) {
	require.Equal(t, 0, PlanLevel(PlanFree))
	require.Equal(t, 1, PlanLevel(PlanPro))
	require.Equal(t, 2, PlanLevel(PlanFlagship))
	require.Equal(t, 0, PlanLevel("unknown"), "unknown plan must degrade to free level")
	require.Equal(t, 0, PlanLevel(""))
}

func TestPlanPriceCents(t *testing.T) {
	require.Equal(t, int64(0), PlanPriceCents(PlanFree))
	require.Equal(t, int64(3900), PlanPriceCents(PlanPro))
	require.Equal(t, int64(9900), PlanPriceCents(PlanFlagship))
	require.Equal(t, int64(0), PlanPriceCents("unknown"))
}

// ===== FeatureGate paywall matrix (CONTRACT §15.6 A2 ③) =====

func TestFeatureGate_FreeDeniedProFeature(t *testing.T) {
	store := NewMemoryStore()
	seedPlan(store, 1, PlanFree) // explicit free record
	g := NewFeatureGate(store)
	for _, f := range []string{FeatureAIAdvice, FeatureMarketingAuto, FeatureGroupbuy, FeatureGroupSOP} {
		ok, err := g.Can(ctxOf(1), f)
		require.NoError(t, err)
		require.False(t, ok, "free must deny pro feature %s", f)
	}
}

func TestFeatureGate_NoRecordDeniedFlagshipFeature(t *testing.T) {
	store := NewMemoryStore() // no record at all → free
	g := NewFeatureGate(store)
	ok, err := g.Can(ctxOf(1), FeatureAttributionBI)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestFeatureGate_ProAllowedProFeature(t *testing.T) {
	store := NewMemoryStore()
	seedPlan(store, 1, PlanPro)
	g := NewFeatureGate(store)
	for _, f := range []string{FeatureAIAdvice, FeatureMarketingAuto, FeatureGroupbuy, FeatureGroupSOP} {
		ok, err := g.Can(ctxOf(1), f)
		require.NoError(t, err)
		require.True(t, ok, "pro must allow %s", f)
	}
}

func TestFeatureGate_ProDeniedFlagshipFeature(t *testing.T) {
	store := NewMemoryStore()
	seedPlan(store, 1, PlanPro)
	g := NewFeatureGate(store)
	for _, f := range []string{FeatureAttributionBI, FeatureMultiStore, FeatureAdvancedReport} {
		ok, err := g.Can(ctxOf(1), f)
		require.NoError(t, err)
		require.False(t, ok, "pro must deny flagship feature %s", f)
	}
}

func TestFeatureGate_FlagshipAllAllowed(t *testing.T) {
	store := NewMemoryStore()
	seedPlan(store, 1, PlanFlagship)
	g := NewFeatureGate(store)
	feats := []string{
		FeatureAIAdvice, FeatureMarketingAuto, FeatureGroupbuy, FeatureGroupSOP,
		FeatureAttributionBI, FeatureMultiStore, FeatureAdvancedReport,
	}
	for _, f := range feats {
		ok, err := g.Can(ctxOf(1), f)
		require.NoError(t, err)
		require.True(t, ok, "flagship must allow %s", f)
	}
}

func TestFeatureGate_UnknownFeatureFalse(t *testing.T) {
	store := NewMemoryStore()
	seedPlan(store, 1, PlanFlagship) // even flagship doesn't grant unknown
	g := NewFeatureGate(store)
	ok, err := g.Can(ctxOf(1), "no_such_feature")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestFeatureGate_RequireUpgradeRequired(t *testing.T) {
	store := NewMemoryStore()
	seedPlan(store, 1, PlanFree)
	g := NewFeatureGate(store)
	err := g.Require(ctxOf(1), FeatureAIAdvice)
	require.ErrorIs(t, err, ErrPlanUpgradeRequired)
}

func TestFeatureGate_RequirePassOnAllowed(t *testing.T) {
	store := NewMemoryStore()
	seedPlan(store, 1, PlanPro)
	g := NewFeatureGate(store)
	require.NoError(t, g.Require(ctxOf(1), FeatureAIAdvice))
}

func TestFeatureGate_CrossTenant(t *testing.T) {
	store := NewMemoryStore()
	seedPlan(store, 1, PlanPro) // tenant 1 is pro
	g := NewFeatureGate(store)
	// tenant 2 has no subscription → free → pro feature denied (no leakage).
	ok, err := g.Can(ctxOf(2), FeatureAIAdvice)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestFeatureGate_NoTenantInCtxErrors(t *testing.T) {
	store := NewMemoryStore()
	g := NewFeatureGate(store)
	ok, err := g.Can(context.Background(), FeatureAIAdvice)
	require.False(t, ok)
	require.ErrorIs(t, err, multitenancy.ErrNoTenant)
}
