package rule

import (
	"fmt"

	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
	"github.com/zhangguijunshi/backend/internal/margin"
)

// ApplyGuardrail validates a single (inv, rule) against the margin floor and
// produces a RoutedItem. It is the rule stage's hard constraint: any pricing
// action must respect the floor; the agent/exec stages re-check the same guard
// (defense-in-depth, CONTRACT §12.5.4).
//
// Returns (zero, false) — "do not emit a pricing action" — when:
//   - inv.CostCents <= 0 || inv.PriceCents <= 0 (no cost data → no pricing
//     action, prevents hallucinated loss);
//   - the rule's ReasonType has no frozen ReasonToAction entry (defensive);
//   - even the rule's LEAST-aggressive discount would breach the floor
//     (unsalvageable — the product is too tight-margin for this rule).
//
// On success the ParamRange is clamped so any discount the LLM picks respects
// the floor, and the nominal CostCeilCents = cost*(100+floor)/100 is filled.
// All amounts are int64 cents; the floor is an integer pct (5=5%); the guard
// library's own truncation is cleared by clamping to the "safe" floor price.
func ApplyGuardrail(inv signal.InventorySig, r Rule, guard margin.Guard) (RoutedItem, bool) {
	cost := inv.CostCents
	price := inv.PriceCents
	if cost <= 0 || price <= 0 {
		return RoutedItem{}, false
	}
	actionType, ok := ReasonToAction[r.Action.ReasonType]
	if !ok {
		return RoutedItem{}, false
	}

	floor := guard.MarginFloorPct

	// Nominal cost ceil (declarative field per CONTRACT §12.3.3 formula).
	costCeil := cost * (100 + floor) / 100
	if costCeil < cost {
		costCeil = cost // defensive: floor<0 would lower the ceil; never let it dip below cost
	}

	pr := r.Action.ParamRange
	clampedMin := pr.DiscountMinPermille
	maxPermille := pr.DiscountMaxPermille
	if maxPermille <= 0 {
		maxPermille = 1000 // unset upper bound → treat as原价 (no discount)
	}

	if floor > 0 {
		// Safe floor price: smallest newPrice (in cents) such that the margin
		// library's ValidatePriceAdj passes despite its integer truncation.
		// Solve (newPrice-cost)/newPrice*100 >= floor  ⟺  newPrice >= 100*cost/(100-floor).
		den := 100 - floor
		if den <= 0 {
			den = 1 // floor>=100: unsatisfiable; treat den=1 so safePrice=cost*100 (huge), rejected below
		}
		safePrice := (cost*100 + den - 1) / den // ceil(100*cost/(100-floor))
		if safePrice < costCeil {
			safePrice = costCeil
		}
		// Floor-respecting min permille: price*P/1000 >= safePrice.
		floorPermille := (safePrice*1000 + price - 1) / price // ceil
		if floorPermille > 1000 {
			floorPermille = 1000
		}
		// Unsalvageable: even the least-aggressive discount (max permille =
		// highest customer price) breaches the floor.
		if floorPermille > maxPermille {
			return RoutedItem{}, false
		}
		if clampedMin < floorPermille {
			clampedMin = floorPermille // clamp the most-aggressive discount back to the floor
		}
		// Defense-in-depth: re-confirm with the margin library at the safe price.
		if err := margin.ValidatePriceAdj(guard, cost, safePrice); err != nil {
			return RoutedItem{}, false // fail closed (should not happen given safePrice derivation)
		}
	}
	if clampedMin > maxPermille {
		clampedMin = maxPermille
	}

	return RoutedItem{
		RuleID:         r.ID,
		ReasonType:     r.Action.ReasonType,
		ActionType:     actionType,
		Target:         r.Action.Target,
		ProductID:      inv.ProductID,
		Product:        inv.Name,
		CostCents:      cost,
		PriceCents:     price,
		CostCeilCents:  costCeil,
		MarginFloorPct: floor,
		Priority:       r.Priority,
		Reason:         reasonFor(r.Action.ReasonType, inv.Name, floor, clampedMin),
		ParamRange: ParamRange{
			DiscountMinPermille: clampedMin,
			DiscountMaxPermille: pr.DiscountMaxPermille,
			ThresholdCents:      pr.ThresholdCents,
			OffCents:            pr.OffCents,
			LimitQtyMax:         pr.LimitQtyMax,
			ExpireHours:         pr.ExpireHours,
		},
	}, true
}

// reasonFor renders a short boss-readable reason; values are substituted once
// so no placeholder leaks.
func reasonFor(reasonType, productName string, floorPct, minPermille int64) string {
	if productName == "" {
		productName = "-"
	}
	return fmt.Sprintf("%s: %s 毛利护栏通过(下限%d%%,折扣下限%d‰)", reasonType, productName, floorPct, minPermille)
}
