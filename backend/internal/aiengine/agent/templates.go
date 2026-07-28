package agent

import "github.com/zhangguijunshi/backend/internal/aiengine/rule"

// TemplateCopy returns the deterministic fallback copy (≤40 字) for an
// ActionType, used when the LLM is unavailable or returned unusable output.
// Templates are clean of sensitive / advertising-law absolutism words so the
// downstream contentfilter is a no-op on them; output.Service also re-runs the
// filter on LLM-produced copy and falls back here when the filter hard-blocks.
//
// The strings stay short and colloquial (boss-facing WeCom / moments copy).
func TemplateCopy(actionType string) string {
	switch actionType {
	case rule.ActGroupSeckill:
		return "群秒杀限时特惠 手慢无"
	case rule.ActCouponWake:
		return "专属券已到账 回店可用"
	case rule.ActMomentsPost:
		return "今日好物推荐 快来看看"
	case rule.ActPriceAdj:
		return "今日特价 来店享优惠"
	case rule.ActGroupbuy:
		return "拼团享好价 拉人成团"
	case rule.ActRechargePromo:
		return "储值优惠 充值享好礼"
	default:
		return "今日推荐 快来看看"
	}
}

// TemplateParams builds the conservative fallback params from a RoutedItem's
// ParamRange lower bounds (CONTRACT §12.3.5: "params 取 ParamRange 下限"). For
// discount actions bound to a real product it emits the floor discount permille
// (already clamped to the margin floor by rule.ApplyGuardrail) plus the derived
// floor price; for full-reduction actions it emits the threshold / off / limit
// / expire lower bounds. The map is never nil so downstream JSON encoding is
// stable.
func TemplateParams(ri rule.RoutedItem) map[string]any {
	pr := ri.ParamRange
	p := make(map[string]any)
	if ri.CostCents > 0 && ri.PriceCents > 0 && pr.DiscountMinPermille > 0 {
		p["discount_permille"] = pr.DiscountMinPermille
		p["price_after_cents"] = ri.PriceCents * pr.DiscountMinPermille / 1000
	}
	if pr.ThresholdCents > 0 {
		p["threshold_cents"] = pr.ThresholdCents
	}
	if pr.OffCents > 0 {
		p["off_cents"] = pr.OffCents
	}
	if pr.LimitQtyMax > 0 {
		p["limit_qty"] = pr.LimitQtyMax
	}
	if pr.ExpireHours > 0 {
		p["expire_hours"] = pr.ExpireHours
	}
	return p
}

// templateItem is the full deterministic fallback for one RoutedItem: template
// copy + template params, MarginOK true (the rule stage already guaranteed the
// floor via the clamped DiscountMinPermille; full-reduction actions have
// cost=0 and pass trivially).
func templateItem(ri rule.RoutedItem) EnhancedItem {
	return EnhancedItem{
		Routed:   ri,
		Copy:     TemplateCopy(ri.ActionType),
		Params:   TemplateParams(ri),
		MarginOK: true,
	}
}
