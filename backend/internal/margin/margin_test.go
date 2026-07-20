package margin

import (
	"errors"
	"testing"
)

func TestValidateCoupon_FullReduction(t *testing.T) {
	g := Guard{MarginFloorPct: 20}
	rule := CouponRule{Type: typeFullReduction, ThresholdCents: 1000, OffCents: 200}
	// price 1000, cost 700: pay=800, margin=(800-700)/800*100=12.5% → 12 < 20 → 拒
	products := []ProductView{{ID: 1, CostCents: 700, PriceCents: 1000}}
	if err := ValidateCoupon(g, rule, products); !errors.Is(err, ErrBelowMarginFloor) {
		t.Fatalf("expect ErrBelowMarginFloor, got %v", err)
	}
	// price 1000, cost 500: pay=800, margin=(800-500)/800*100=37% → 通过
	products = []ProductView{{ID: 1, CostCents: 500, PriceCents: 1000}}
	if err := ValidateCoupon(g, rule, products); err != nil {
		t.Fatalf("expect nil, got %v", err)
	}
	// 未达门槛: price 900 < threshold 1000 → pay=900(原价), margin=(900-700)/900=22% → 通过
	products = []ProductView{{ID: 1, CostCents: 700, PriceCents: 900}}
	if err := ValidateCoupon(g, rule, products); err != nil {
		t.Fatalf("below-threshold should not trigger, got %v", err)
	}
}

func TestValidateCoupon_Discount(t *testing.T) {
	g := Guard{MarginFloorPct: 15}
	rule := CouponRule{Type: typeDiscount, DiscountPermille: 850} // 8.5折
	// price 1000, cost 800: pay=850, margin=(850-800)/850=5% → 拒
	products := []ProductView{{ID: 1, CostCents: 800, PriceCents: 1000}}
	if err := ValidateCoupon(g, rule, products); !errors.Is(err, ErrBelowMarginFloor) {
		t.Fatalf("expect ErrBelowMarginFloor, got %v", err)
	}
	// price 1000, cost 700: pay=850, margin=(850-700)/850=17% → 通过
	products = []ProductView{{ID: 1, CostCents: 700, PriceCents: 1000}}
	if err := ValidateCoupon(g, rule, products); err != nil {
		t.Fatalf("expect nil, got %v", err)
	}
}

func TestValidateCoupon_Cash(t *testing.T) {
	g := Guard{MarginFloorPct: 10}
	rule := CouponRule{Type: typeCash, OffCents: 500}
	// price 1000, cost 600: pay=500, margin=(500-600)/500=-20% → 拒(亏本)
	products := []ProductView{{ID: 1, CostCents: 600, PriceCents: 1000}}
	if err := ValidateCoupon(g, rule, products); !errors.Is(err, ErrBelowMarginFloor) {
		t.Fatalf("expect ErrBelowMarginFloor, got %v", err)
	}
	// price 1000, cost 400: pay=500, margin=(500-400)/500=20% → 通过
	products = []ProductView{{ID: 1, CostCents: 400, PriceCents: 1000}}
	if err := ValidateCoupon(g, rule, products); err != nil {
		t.Fatalf("expect nil, got %v", err)
	}
}

func TestValidateCoupon_Gift(t *testing.T) {
	g := Guard{MarginFloorPct: 30}
	rule := CouponRule{Type: typeGift}
	// gift 不改实付: pay=price, margin 按原价算
	products := []ProductView{{ID: 1, CostCents: 700, PriceCents: 1000}} // margin=30% → 通过
	if err := ValidateCoupon(g, rule, products); err != nil {
		t.Fatalf("gift at exactly floor should pass, got %v", err)
	}
	products = []ProductView{{ID: 1, CostCents: 800, PriceCents: 1000}} // margin=20% → 拒
	if err := ValidateCoupon(g, rule, products); !errors.Is(err, ErrBelowMarginFloor) {
		t.Fatalf("expect ErrBelowMarginFloor, got %v", err)
	}
}

func TestValidateCoupon_FloorZero_DisablesCheck(t *testing.T) {
	g := Guard{MarginFloorPct: 0}
	rule := CouponRule{Type: typeCash, OffCents: 10000} // 巨额抵扣, 必亏
	products := []ProductView{{ID: 1, CostCents: 1000, PriceCents: 1000}}
	if err := ValidateCoupon(g, rule, products); err != nil {
		t.Fatalf("floor=0 should disable, got %v", err)
	}
}

func TestValidateCoupon_EmptyProducts(t *testing.T) {
	g := Guard{MarginFloorPct: 50}
	if err := ValidateCoupon(g, CouponRule{Type: typeDiscount, DiscountPermille: 100}, nil); err != nil {
		t.Fatalf("empty products should return nil, got %v", err)
	}
}

func TestValidateCoupon_AnyProductBelowFloor_Rejects(t *testing.T) {
	g := Guard{MarginFloorPct: 20}
	rule := CouponRule{Type: typeDiscount, DiscountPermille: 850}
	products := []ProductView{
		{ID: 1, CostCents: 500, PriceCents: 1000}, // margin 17% → 通过单看
		{ID: 2, CostCents: 800, PriceCents: 1000}, // margin 5% → 拒
	}
	if err := ValidateCoupon(g, rule, products); !errors.Is(err, ErrBelowMarginFloor) {
		t.Fatalf("any below-floor product should reject, got %v", err)
	}
}

func TestValidatePriceAdj(t *testing.T) {
	g := Guard{MarginFloorPct: 20}
	// cost 800, new price 900: margin=(900-800)/900=11% → 拒
	if err := ValidatePriceAdj(g, 800, 900); !errors.Is(err, ErrBelowMarginFloor) {
		t.Fatalf("expect ErrBelowMarginFloor, got %v", err)
	}
	// cost 800, new price 1100: margin=(1100-800)/1100=27% → 通过
	if err := ValidatePriceAdj(g, 800, 1100); err != nil {
		t.Fatalf("expect nil, got %v", err)
	}
	// floor 0 禁用
	if err := ValidatePriceAdj(Guard{}, 1000, 1); err != nil {
		t.Fatalf("floor=0 disables, got %v", err)
	}
}
