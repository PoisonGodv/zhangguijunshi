package rule

// DefaultSeeds returns the in-code cold-start rule seeds covering all 5 business
// types (fruit / convenience / maternity / fresh / apparel) and all 7 ReasonTypes
// (clearance / rain / bundle / wake / festival / birthday / high_value). Mapping
// per CONTRACT §12.3.0: clearance/rain/bundle → group_seckill (inventory-linked);
// wake/festival/birthday/high_value → coupon_wake (placeholder).
//
// Priority is larger-first. MarginFloorPct is declarative per-rule; enforcement
// uses the common margin.Guard injected at NewEngine (CONTRACT §12.5.4).
//
// ParamRange int64 semantics: discount in permille (500=5折, 700=7折, 900=9折);
// ThresholdCents/OffCents in cents for full-reduction placeholder actions.
func DefaultSeeds() []Rule {
	return []Rule{
		// ===== fruit: 临期清仓 / 雨天秒杀 / 滞销捆绑 =====
		{
			ID:       "clearance_fruit_3d",
			Business: "fruit",
			Priority: 50,
			Enable:   true,
			Conds: []Cond{
				{Field: "inventory.days_to_expiry", Op: "between", Value: []int{1, 3}},
			},
			Action: Action{
				ReasonType: ReasonClearanceCoupon,
				Target:     "临期SKU群秒杀",
				ParamRange: ParamRange{DiscountMinPermille: 500, DiscountMaxPermille: 700, LimitQtyMax: 50},
			},
			MarginFloorPct: 5,
		},
		{
			ID:       "rain_fruit",
			Business: "fruit",
			Priority: 40,
			Enable:   true,
			Conds: []Cond{
				{Field: "weather.condition", Op: "eq", Value: "rain"},
			},
			Action: Action{
				ReasonType: ReasonRainDelivery,
				Target:     "群内秒杀+本小区配送",
				ParamRange: ParamRange{DiscountMinPermille: 700, DiscountMaxPermille: 900, ThresholdCents: 6800, LimitQtyMax: 50},
			},
			MarginFloorPct: 5,
		},
		{
			ID:       "bundle_fruit",
			Business: "fruit",
			Priority: 30,
			Enable:   true,
			Conds: []Cond{
				{Field: "inventory.slow_moving", Op: "eq", Value: true},
			},
			Action: Action{
				ReasonType: ReasonBundleSlow,
				Target:     "滞销+畅销捆绑",
				ParamRange: ParamRange{DiscountMinPermille: 920, DiscountMaxPermille: 920},
			},
			MarginFloorPct: 8,
		},

		// ===== fresh: 雨天配送 =====
		{
			ID:       "rain_fresh",
			Business: "fresh",
			Priority: 40,
			Enable:   true,
			Conds: []Cond{
				{Field: "weather.condition", Op: "eq", Value: "rain"},
			},
			Action: Action{
				ReasonType: ReasonRainDelivery,
				Target:     "本小区满68免费送上门",
				ParamRange: ParamRange{DiscountMinPermille: 750, DiscountMaxPermille: 900, ThresholdCents: 6800, LimitQtyMax: 50},
			},
			MarginFloorPct: 5,
		},

		// ===== convenience: 滞销捆绑 =====
		{
			ID:       "bundle_convenience",
			Business: "convenience",
			Priority: 30,
			Enable:   true,
			Conds: []Cond{
				{Field: "inventory.slow_moving", Op: "eq", Value: true},
			},
			Action: Action{
				ReasonType: ReasonBundleSlow,
				Target:     "滞销品捆绑畅销品",
				ParamRange: ParamRange{DiscountMinPermille: 920, DiscountMaxPermille: 950},
			},
			MarginFloorPct: 8,
		},

		// ===== maternity: 沉睡唤醒 / 生日关怀 =====
		{
			ID:       "wake_maternity",
			Business: "maternity",
			Priority: 20,
			Enable:   true,
			Conds: []Cond{
				{Field: "members.sleep_30", Op: "gte", Value: 10},
			},
			Action: Action{
				ReasonType: ReasonWakeDormant,
				Target:     "30天未到店高价值会员",
				ParamRange: ParamRange{ThresholdCents: 3000, OffCents: 500, ExpireHours: 48},
			},
			MarginFloorPct: 0,
		},
		{
			ID:       "birthday_maternity",
			Business: "maternity",
			Priority: 10,
			Enable:   true,
			Conds: []Cond{
				{Field: "members.birthday_today", Op: "in", Value: "nonempty"},
			},
			Action: Action{
				ReasonType: ReasonBirthday,
				Target:     "当天生日会员",
				ParamRange: ParamRange{ThresholdCents: 5000, OffCents: 800},
			},
			MarginFloorPct: 0,
		},

		// ===== apparel: 高毛利推送 / 换季满减 =====
		{
			ID:       "high_value_apparel",
			Business: "apparel",
			Priority: 70,
			Enable:   true,
			Conds: []Cond{
				{Field: "members.high_value", Op: "in", Value: "nonempty"},
			},
			Action: Action{
				ReasonType: ReasonHighValue,
				Target:     "高价值会员专享券",
				ParamRange: ParamRange{DiscountMinPermille: 900, DiscountMaxPermille: 900},
			},
			MarginFloorPct: 15,
		},
		{
			ID:       "festival_apparel",
			Business: "apparel",
			Priority: 60,
			Enable:   true,
			Conds: []Cond{
				{Field: "holiday.days_to", Op: "between", Value: []int{0, 7}},
				{Field: "holiday.is_festival", Op: "eq", Value: true},
			},
			Action: Action{
				ReasonType: ReasonFestival,
				Target:     "换季主题满减",
				ParamRange: ParamRange{ThresholdCents: 9900, OffCents: 1500},
			},
			MarginFloorPct: 5,
		},
	}
}
