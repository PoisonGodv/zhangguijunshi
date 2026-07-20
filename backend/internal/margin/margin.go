// Package margin implements the gross-margin guardrail (毛利护栏, AI-02 seed,
// CONTRACT §10.3). It is a PURE-FUNCTION package: it imports NO domain packages
// (marketing / catalog / billing). The main thread adapts a marketing.Coupon to
// margin.CouponRule and supplies the product cost/price view at the call site.
//
// The guard enforces the plan-book margin floor (计划书 7.4 合规护栏): any coupon
// issuance or price adjustment that would drop a product's margin below the
// configured floor is rejected. Amounts are int64 cents; discount is permille
// (‰, 850 = 85% = 8.5折); the floor is an integer percentage (10 = 10%). No
// floating point is used anywhere.
package margin

import "errors"

// ErrBelowMarginFloor is returned when applying a coupon or price change would
// drop the margin below the configured floor.
var ErrBelowMarginFloor = errors.New("margin: below floor")

// Guard carries the margin-floor configuration. MarginFloorPct is the minimum
// acceptable gross-margin percentage (10 = 10%); 0 disables the check.
type Guard struct {
	MarginFloorPct int64
}

// CouponRule is margin's mirror of a coupon's elastic params (anti-corruption:
// marketing.CouponParams is adapted into this by the main thread). Fields are
// used according to Type, matching the marketing.CouponParams contract.
type CouponRule struct {
	Type             string // full_reduction|discount|cash|gift
	ThresholdCents   int64
	OffCents         int64
	DiscountPermille int64 // 0-1000
}

// ProductView is the per-product cost/price view the guard evaluates against.
type ProductView struct {
	ID                    int64
	CostCents, PriceCents int64
}

// Coupon type constants — kept unexported to avoid exporting a string-typed
// enum; the caller passes the same Type string marketing uses.
const (
	typeFullReduction = "full_reduction"
	typeDiscount      = "discount"
	typeCash          = "cash"
	typeGift          = "gift"
)

// effectivePay computes the amount a customer actually pays for one product
// after applying the coupon rule. Returns the post-discount price in cents.
//
//	full_reduction: price>=threshold → price-off; otherwise price (未达门槛, 不触发)
//	discount:       price * permille / 1000
//	cash:           max(price-off, 0)
//	gift:           price (赠品成本 M2 不计, 按原价)
//
// An unknown type leaves the price unchanged (defensive: the caller validates
// the type at the marketing layer).
func effectivePay(rule CouponRule, priceCents int64) int64 {
	switch rule.Type {
	case typeFullReduction:
		if priceCents >= rule.ThresholdCents && rule.ThresholdCents > 0 {
			return priceCents - rule.OffCents
		}
		return priceCents
	case typeDiscount:
		if rule.DiscountPermille <= 0 {
			return priceCents
		}
		if rule.DiscountPermille > 1000 {
			return priceCents // 防御: 超出 1000 当原价(调用方应校验)
		}
		return priceCents * rule.DiscountPermille / 1000
	case typeCash:
		pay := priceCents - rule.OffCents
		if pay < 0 {
			return 0
		}
		return pay
	case typeGift:
		return priceCents
	default:
		return priceCents
	}
}

// marginPct computes the gross-margin percentage of a sale: (pay-cost)/pay*100.
// A non-positive pay with a positive cost is a guaranteed floor breach (returns
// a large negative so the comparison fails); pay==0 && cost==0 reads as 100%
// (free item, no cost — acceptable).
func marginPct(pay, cost int64) int64 {
	if pay <= 0 {
		if cost > 0 {
			return -1 // 亏本, 必触发护栏
		}
		return 100 // pay=0 cost=0, 视为满毛利
	}
	return (pay - cost) * 100 / pay
}

// ValidateCoupon checks that applying rule to EVERY product in products keeps
// the gross margin at or above g.MarginFloorPct. A floor of 0 disables the
// check (returns nil). An empty products list returns nil (nothing to validate
// — the caller owns the "which products" decision). Any product below the floor
// yields ErrBelowMarginFloor.
func ValidateCoupon(g Guard, rule CouponRule, products []ProductView) error {
	if g.MarginFloorPct <= 0 {
		return nil
	}
	for _, p := range products {
		pay := effectivePay(rule, p.PriceCents)
		if marginPct(pay, p.CostCents) < g.MarginFloorPct {
			return ErrBelowMarginFloor
		}
	}
	return nil
}

// ValidatePriceAdj checks that a price change to newPriceCents keeps the margin
// at or above the floor: (newPrice-cost)/newPrice*100 >= floor. A floor of 0
// disables the check. A non-positive new price with a positive cost breaches.
func ValidatePriceAdj(g Guard, costCents, newPriceCents int64) error {
	if g.MarginFloorPct <= 0 {
		return nil
	}
	if marginPct(newPriceCents, costCents) < g.MarginFloorPct {
		return ErrBelowMarginFloor
	}
	return nil
}
