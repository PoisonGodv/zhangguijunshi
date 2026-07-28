package rule

// ActionType — 6 execution channels (persisted to strategy_items.action_type,
// aligned with the ch05 CHECK constraint). Defined here in the rule package so
// the exec stage can `import rule` and dispatch on these constants; exec MUST
// NOT look at ReasonType (CONTRACT §12.5.9: action_type two-layer non-drift).
const (
	ActGroupSeckill  = "group_seckill"  // 群秒杀 (临期/雨天/滞销捆绑) → 发券+群发
	ActCouponWake    = "coupon_wake"    // 券唤醒私聊 (沉睡/节日/生日/高价值) → 发券+1v1
	ActMomentsPost   = "moments_post"   // 朋友圈 (高毛利推送) → 文案 (M3-S1 audit 不实发)
	ActPriceAdj      = "price_adj"      // 改价 (高危, 需二次确认) → catalog.UpdatePrice
	ActGroupbuy      = "groupbuy"       // 拼团 → groupbuy.Create
	ActRechargePromo = "recharge_promo" // 储值促销 → stub (M4)
)

// ReasonType — 7 rule semantics (persisted to strategy_items.reason_type, the
// rule library's "why"). The rule layer declares ReasonType; ActionType is
// derived via ReasonToAction (never hand-filled in a Rule).
const (
	ReasonClearanceCoupon = "clearance_coupon"  // 临期清仓
	ReasonRainDelivery    = "rain_delivery"     // 雨天配送
	ReasonBundleSlow      = "bundle_slow"       // 滞销捆绑
	ReasonWakeDormant     = "wake_dormant"      // 沉睡唤醒
	ReasonFestival        = "festival_discount" // 节日满减
	ReasonBirthday        = "birthday_care"     // 生日关怀
	ReasonHighValue       = "high_value_push"   // 高毛利推送
)

// ReasonToAction is the frozen ReasonType→ActionType mapping. The rule stage
// produces ActionType exclusively via this map. M3-S1 fixed mapping; the
// apparel business may route high_value to moments_post in a second-pass
// mapping deferred to M3-S3.
var ReasonToAction = map[string]string{
	ReasonClearanceCoupon: ActGroupSeckill,
	ReasonRainDelivery:    ActGroupSeckill,
	ReasonBundleSlow:      ActGroupSeckill,
	ReasonWakeDormant:     ActCouponWake,
	ReasonFestival:        ActCouponWake,
	ReasonBirthday:        ActCouponWake,
	ReasonHighValue:       ActCouponWake,
}
