package subscription

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

func TestService_CurrentPlan_DefaultFree(t *testing.T) {
	svc := NewService(NewMemoryStore(), WithNowFunc(func() time.Time { return fixedNow }))
	plan, err := svc.CurrentPlan(ctxOf(1))
	require.NoError(t, err)
	require.Equal(t, PlanFree, plan)
}

func TestService_GetSubscription_None_Nil(t *testing.T) {
	svc := NewService(NewMemoryStore(), WithNowFunc(func() time.Time { return fixedNow }))
	sub, err := svc.GetSubscription(ctxOf(1))
	require.NoError(t, err)
	require.Nil(t, sub)
}

func TestService_GateExposed(t *testing.T) {
	svc := NewService(NewMemoryStore())
	require.NotNil(t, svc.Gate())
}

func TestService_Subscribe_Pro_DefaultOneMonth(t *testing.T) {
	svc := NewService(NewMemoryStore(), WithNowFunc(func() time.Time { return fixedNow }))
	sub, err := svc.Subscribe(ctxOf(1), SubscribeInput{Plan: PlanPro, Source: "manual"})
	require.NoError(t, err)
	require.Equal(t, int64(1), sub.TenantID)
	require.Equal(t, PlanPro, sub.Plan)
	require.Equal(t, "active", sub.Status)
	require.Equal(t, "manual", sub.Source)
	require.NotNil(t, sub.PeriodEndsAt, "paid plan must set a period end")
	require.True(t, sub.PeriodEndsAt.Equal(fixedNow.AddDate(0, 1, 0)), "default pro period = 1 month")

	// plan visible via CurrentPlan / gate.
	plan, err := svc.CurrentPlan(ctxOf(1))
	require.NoError(t, err)
	require.Equal(t, PlanPro, plan)
	require.NoError(t, svc.Gate().Require(ctxOf(1), FeatureAIAdvice))
}

func TestService_Subscribe_Pro_CustomMonths(t *testing.T) {
	svc := NewService(NewMemoryStore(), WithNowFunc(func() time.Time { return fixedNow }))
	sub, err := svc.Subscribe(ctxOf(1), SubscribeInput{Plan: PlanPro, PeriodMonths: 12})
	require.NoError(t, err)
	require.True(t, sub.PeriodEndsAt.Equal(fixedNow.AddDate(0, 12, 0)))
}

func TestService_Subscribe_Free_NoPeriodEnd(t *testing.T) {
	svc := NewService(NewMemoryStore(), WithNowFunc(func() time.Time { return fixedNow }))
	sub, err := svc.Subscribe(ctxOf(1), SubscribeInput{Plan: PlanFree})
	require.NoError(t, err)
	require.Equal(t, PlanFree, sub.Plan)
	require.Nil(t, sub.PeriodEndsAt, "free is permanent")
}

func TestService_Subscribe_SwitchCancelsPrior(t *testing.T) {
	store := NewMemoryStore()
	svc := NewService(store, WithNowFunc(func() time.Time { return fixedNow }))

	_, err := svc.Subscribe(ctxOf(1), SubscribeInput{Plan: PlanPro})
	require.NoError(t, err)
	sub, err := svc.Subscribe(ctxOf(1), SubscribeInput{Plan: PlanFlagship})
	require.NoError(t, err)
	require.Equal(t, PlanFlagship, sub.Plan)

	hist, err := store.ListHistory(context.Background(), 1)
	require.NoError(t, err)
	require.Len(t, hist, 2)
	require.Equal(t, "cancelled", hist[0].Status)
	require.Equal(t, "active", hist[1].Status)
}

func TestService_Subscribe_NoTenant_ErrNoTenant(t *testing.T) {
	svc := NewService(NewMemoryStore(), WithNowFunc(func() time.Time { return fixedNow }))
	_, err := svc.Subscribe(context.Background(), SubscribeInput{Plan: PlanPro})
	require.ErrorIs(t, err, multitenancy.ErrNoTenant)
}

func TestService_ConfirmPayment_Pro(t *testing.T) {
	store := NewMemoryStore()
	svc := NewService(store, WithNowFunc(func() time.Time { return fixedNow }))
	sub, err := svc.ConfirmPayment(context.Background(), 1, PlanPriceCents(PlanPro), "wxpay", "OUT_001")
	require.NoError(t, err)
	require.Equal(t, PlanPro, sub.Plan)
	require.Equal(t, "wxpay", sub.Source)
	require.Equal(t, "OUT_001", sub.PaymentRef)
	require.True(t, sub.PeriodEndsAt.Equal(fixedNow.AddDate(0, 1, 0)))

	// tenant is now pro via explicit-tenantID activation path.
	plan, err := store.CurrentPlan(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, PlanPro, plan)
}

func TestService_ConfirmPayment_Flagship(t *testing.T) {
	svc := NewService(NewMemoryStore(), WithNowFunc(func() time.Time { return fixedNow }))
	sub, err := svc.ConfirmPayment(context.Background(), 1, PlanPriceCents(PlanFlagship), "alipay", "OUT_002")
	require.NoError(t, err)
	require.Equal(t, PlanFlagship, sub.Plan)
	require.Equal(t, "alipay", sub.Source)
}

func TestService_ConfirmPayment_UnknownAmount_ErrParam(t *testing.T) {
	svc := NewService(NewMemoryStore(), WithNowFunc(func() time.Time { return fixedNow }))
	cases := []int64{0, 1, 3899, 3901, 10000, -1}
	for _, amt := range cases {
		_, err := svc.ConfirmPayment(context.Background(), 1, amt, "wxpay", "OUT_X")
		require.ErrorIs(t, err, errcode.ErrParam, "amount %d must not map to a plan", amt)
	}
}

func TestService_CrossTenant(t *testing.T) {
	store := NewMemoryStore()
	svc := NewService(store, WithNowFunc(func() time.Time { return fixedNow }))

	// tenant 1 subscribes pro.
	_, err := svc.Subscribe(ctxOf(1), SubscribeInput{Plan: PlanPro})
	require.NoError(t, err)

	// tenant 2 is unaffected → free, denied pro feature (no leakage).
	plan, err := svc.CurrentPlan(ctxOf(2))
	require.NoError(t, err)
	require.Equal(t, PlanFree, plan)
	require.ErrorIs(t, svc.Gate().Require(ctxOf(2), FeatureAIAdvice), ErrPlanUpgradeRequired)

	// ConfirmPayment under tenant 3 does not touch tenant 1/2.
	_, err = svc.ConfirmPayment(context.Background(), 3, PlanPriceCents(PlanFlagship), "wxpay", "OUT_3")
	require.NoError(t, err)
	plan, err = svc.CurrentPlan(ctxOf(1))
	require.NoError(t, err)
	require.Equal(t, PlanPro, plan, "tenant 1 plan unchanged by tenant 3 activation")
}
