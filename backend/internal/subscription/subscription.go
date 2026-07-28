// Package subscription implements the tenant plan paywall (BE-B-13): the
// three-tier plan matrix (free/pro/flagship), the feature gate that enforces
// it, and the subscription state store (Memory dev/test + PG production).
//
// It has zero internal cross-domain dependencies (CONTRACT §15.3.0): payment,
// billing and membership never import it, and it never imports them. Paid
// activation flows in through Service.ConfirmPayment (a Fulfiller adapter
// port that maps an amount to a plan); plan/feature checks flow through
// FeatureGate.Can/Require. The tenant is always resolved from context via the
// multitenancy chokepoint (ARCH-01), so a ctx-only call without a resolved
// tenant is impossible by construction.
package subscription

import (
	"context"
	"errors"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// plan 常量 (与 tenants.plan CHECK 一致, CONTRACT §15.3.2).
const (
	PlanFree     = "free"
	PlanPro      = "pro"      // L2 营销版 39元/月或365元/年
	PlanFlagship = "flagship" // L3 旗舰版
)

// 功能常量 (FeatureGate 检查 key; 计划书 §8.1 三层蛋糕功能矩阵).
const (
	FeatureAIAdvice       = "ai_advice"       // 今日3件事 (pro+)
	FeatureMarketingAuto  = "marketing_auto"  // 自动化营销 (pro+)
	FeatureGroupbuy       = "groupbuy"        // 拼团裂变 (pro+)
	FeatureGroupSOP       = "group_sop"       // 社群SOP (pro+)
	FeatureAttributionBI  = "attribution_bi"  // 归因看板 (flagship)
	FeatureMultiStore     = "multi_store"     // 多店 (flagship)
	FeatureAdvancedReport = "advanced_report" // 高级报表 (flagship)
)

// featureMatrix feature→最低 PlanLevel (free 默认满足不列, CONTRACT §15.3.2).
// 未知 feature 查表得 ok=false → FeatureGate.Can 返回 (false, nil).
var featureMatrix = map[string]int{
	FeatureAIAdvice:       PlanLevel(PlanPro),
	FeatureMarketingAuto:  PlanLevel(PlanPro),
	FeatureGroupbuy:       PlanLevel(PlanPro),
	FeatureGroupSOP:       PlanLevel(PlanPro),
	FeatureAttributionBI:  PlanLevel(PlanFlagship),
	FeatureMultiStore:     PlanLevel(PlanFlagship),
	FeatureAdvancedReport: PlanLevel(PlanFlagship),
}

// PlanLevel returns the plan rank used for >= comparison: free=0, pro=1,
// flagship=2. Unknown plans map to 0 (free), so an unrecognized plan never
// grants paid features (CONTRACT §15.3.2).
func PlanLevel(plan string) int {
	switch plan {
	case PlanPro:
		return 1
	case PlanFlagship:
		return 2
	default:
		return 0
	}
}

// PlanPriceCents returns the monthly price in fen (分) for a plan: pro=3900,
// flagship=9900, free (or unknown) =0. In-code mapping pending plan-book §8.1
// confirmation (CONTRACT §15.1).
func PlanPriceCents(plan string) int64 {
	switch plan {
	case PlanPro:
		return 3900
	case PlanFlagship:
		return 9900
	default:
		return 0
	}
}

// Subscription is a tenant's subscription state (subscriptions table,
// migration 000013). At most one status=active row exists per tenant (UNIQUE
// partial index uniq_subscriptions_active_one); switching plans cancels the
// prior active row (CONTRACT §15.3.2).
type Subscription struct {
	ID, TenantID         int64
	Plan                 string // free|pro|flagship
	PeriodStartedAt      time.Time
	PeriodEndsAt         *time.Time // nil=永久(仅 free)
	Status               string     // active|expired|cancelled
	Source               string     // manual|wxpay|alipay|admin
	PaymentRef           string     // 关联支付单 out_trade_no
	CreatedAt, UpdatedAt time.Time
}

// ErrPlanUpgradeRequired is returned by FeatureGate.Require when the ctx
// tenant's current plan does not include the requested feature.
var ErrPlanUpgradeRequired = errors.New("subscription: plan upgrade required")

// SubscribeInput parameterizes Service.Subscribe (manual/admin plan switch or
// renewal).
type SubscribeInput struct {
	Plan         string
	PeriodMonths int    // >0 续期; <=0 用默认(pro/flagship=1月, free=永久)
	Source       string // manual|admin
	PaymentRef   string
}

// tenantIDFromCtx resolves the tenant from ctx via the multitenancy chokepoint
// (ARCH-01). Every ctx-only Service/Gate method routes through here so a call
// without a resolved tenant short-circuits before touching the store.
func tenantIDFromCtx(ctx context.Context) (int64, error) {
	return multitenancy.EnforceTenant(ctx)
}
