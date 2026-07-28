package playbook

// seed.go 按 CONTRACT §14.3.2 / ch07 §7.10.2 表种入 5 业态剧本的静态数据.
// Rules 复用 rule.DefaultSeeds() 中同 Business 的规则 (§14.3.2 "复用同 ID 规则,
// 不必重写"); 若某业态在 DefaultSeeds 中不足 2 条 (fresh 仅 1 条), 用本业态
// 特化规则补齐到 ≥2 (DoD §14.6 A2). 文案模板变量用 {product}/{price}/
// {member_tag}/{qty} 占位. LossRate/CashCoef/ColdPriority 严格按 §14.3.2 表.

import "github.com/zhangguijunshi/backend/internal/aiengine/rule"

// allBusinesses 返回固定顺序的 5 业态, 供 NewStore/List 保持确定性顺序.
func allBusinesses() []string {
	return []string{Fruit, Convenience, Maternity, Fresh, Apparel}
}

// businessParams 每业态的损耗率/现金系数/冷启动优先动作 (§14.3.2 表).
type businessParams struct {
	lossRate     float64
	cashCoef     float64
	coldPriority []string
}

// paramsFor 严格按 §14.3.2 表返回业态参数:
//
//	fruit       0.10 / 1.2  [moments_post, group_seckill, groupbuy]
//	convenience 0.015 / 1.2 [coupon_wake, moments_post]
//	maternity   0.02 / 1.2  [coupon_wake, recharge_promo]
//	fresh       0.10 / 1.2  [groupbuy, group_seckill]
//	apparel     0.03 / 1.3  [coupon_wake, groupbuy]
func paramsFor(business string) businessParams {
	switch business {
	case Fruit:
		return businessParams{
			lossRate:     0.10,
			cashCoef:     1.2,
			coldPriority: []string{rule.ActMomentsPost, rule.ActGroupSeckill, rule.ActGroupbuy},
		}
	case Convenience:
		return businessParams{
			lossRate:     0.015,
			cashCoef:     1.2,
			coldPriority: []string{rule.ActCouponWake, rule.ActMomentsPost},
		}
	case Maternity:
		return businessParams{
			lossRate:     0.02,
			cashCoef:     1.2,
			coldPriority: []string{rule.ActCouponWake, rule.ActRechargePromo},
		}
	case Fresh:
		return businessParams{
			lossRate:     0.10,
			cashCoef:     1.2,
			coldPriority: []string{rule.ActGroupbuy, rule.ActGroupSeckill},
		}
	case Apparel:
		return businessParams{
			lossRate:     0.03,
			cashCoef:     1.3,
			coldPriority: []string{rule.ActCouponWake, rule.ActGroupbuy},
		}
	default:
		return businessParams{cashCoef: 1.2}
	}
}

// rulesFor 返回该业态的默认规则集: rule.DefaultSeeds() 中 Business==business 的
// 规则原样复用; 不足 2 条时用本业态特化规则补齐. 返回新切片, 不复用 DefaultSeeds
// 的底层数组 (避免调用方误改共享).
func rulesFor(business string) []rule.Rule {
	out := make([]rule.Rule, 0, 4)
	for _, r := range rule.DefaultSeeds() {
		if r.Business == business {
			out = append(out, r)
		}
	}
	// DoD §14.6 A2: 每业态 Rules≥2. fresh/convenience 在 DefaultSeeds 各仅 1 条,
	// 用本业态特化规则补齐到 ≥2.
	if len(out) < 2 {
		out = append(out, extraRules(business)...)
	}
	return out
}

// extraRules 返回 DefaultSeeds 未覆盖到 ≥2 条的业态的特化补充规则 (reason_type/
// action 取值仍走 rule 包常量, 保持两层映射一致).
func extraRules(business string) []rule.Rule {
	switch business {
	case Fresh:
		return []rule.Rule{
			{
				ID:       "clearance_fresh_eod",
				Business: Fresh,
				Priority: 50,
				Enable:   true,
				Conds: []rule.Cond{
					{Field: "inventory.days_to_expiry", Op: "between", Value: []int{1, 2}},
				},
				Action: rule.Action{
					ReasonType: rule.ReasonClearanceCoupon,
					Target:     "当日鲜品晚间拼团清仓",
					ParamRange: rule.ParamRange{DiscountMinPermille: 600, DiscountMaxPermille: 800, LimitQtyMax: 50},
				},
				MarginFloorPct: 5,
			},
		}
	case Convenience:
		return []rule.Rule{
			{
				ID:       "wake_convenience",
				Business: Convenience,
				Priority: 20,
				Enable:   true,
				Conds: []rule.Cond{
					{Field: "members.sleep_30", Op: "gte", Value: 10},
				},
				Action: rule.Action{
					ReasonType: rule.ReasonWakeDormant,
					Target:     "30天未到店常客券唤醒",
					ParamRange: rule.ParamRange{ThresholdCents: 2000, OffCents: 500, ExpireHours: 48},
				},
				MarginFloorPct: 0,
			},
		}
	default:
		return nil
	}
}

// promptsFor 返回该业态 ColdPriority 涉及的 action_type 文案模板 (变量占位
// {product}/{price}/{member_tag}/{qty}). key 用 rule.Act* 常量.
func promptsFor(business string) map[string]string {
	p := paramsFor(business)
	out := make(map[string]string, len(p.coldPriority))
	for _, act := range p.coldPriority {
		if tpl, ok := promptTemplates[act]; ok {
			out[act] = tpl
		}
	}
	return out
}

// promptTemplates action_type → 通用文案模板. 5 业态共用, 变量占位统一.
var promptTemplates = map[string]string{
	rule.ActMomentsPost:   "【{product}】今日新鲜到货, 老客专享价 ¥{price}, 限量 {qty} 份, 先到先得~",
	rule.ActGroupSeckill:  "【群秒杀】{product} 限时 ¥{price}, 仅 {qty} 份, {member_tag} 优先抢!",
	rule.ActGroupbuy:      "【拼团】{product} 成团价 ¥{price}, {qty} 人成团, 喊邻居一起省!",
	rule.ActCouponWake:    "亲爱的{member_tag}, 好久不见! 送您一张 ¥{price} 唤醒券, {product} 可用, 限 {qty} 天内有效~",
	rule.ActRechargePromo: "【储值有礼】{member_tag} 专享: 充 {price} 送好礼, {product} 通用, 名额限 {qty} 个!",
	rule.ActPriceAdj:      "【调价提醒】{product} 建议调整至 ¥{price}, 守住毛利底线, {qty} 件在售.",
}

// sopFor 返回社群四阶段 (拉群/促活/转化/裂变), 按业态特化打法文案.
func sopFor(business string) [4]SOPStage {
	return [4]SOPStage{
		{Name: "拉群", Play: sopPlay(business, 0)},
		{Name: "促活", Play: sopPlay(business, 1)},
		{Name: "转化", Play: sopPlay(business, 2)},
		{Name: "裂变", Play: sopPlay(business, 3)},
	}
}

// sopPlay 返回某业态某阶段的打法描述. 通用骨架 + 业态侧重.
func sopPlay(business string, stage int) string {
	base := [4]string{
		"门店张贴活码 + 收银台话术引导, 进店客 100% 邀请入群, 首单礼留人",
		"每日早晚两条福利/到货播报, 穿插互动小红包, 保持群活跃不发广告轰炸",
		"结合冷启动优先动作定向发券/拼团, 用 {product} 做钩子单, 追首单转化",
		"老带新裂变: 邀 1 位邻居入群各得券, 拼团/集赞放大, 把老客变推广员",
	}
	focus := map[string][4]string{
		Fruit: {
			"以当日特价水果/试吃为钩子拉群, 强调『群里先有秒杀价』",
			"早发今日到货 + 晚发清仓秒杀预告, 用新鲜度话术促活",
			"用临期秒杀 + 拼团把到店客流沉淀成群内复购",
			"拼团 + 集赞换购, 让宝妈/熟客拉同小区邻居入群",
		},
		Convenience: {
			"以应急便利 + 会员价差异拉群, 强调『群里下单楼下自提』",
			"早咖啡/晚夜宵场景化推送, 节日券定时唤醒",
			"用满减券 + 组合捆绑把高频低客单提客单",
			"储值返利 + 邀新得券, 锁定周边常驻客",
		},
		Maternity: {
			"以育儿知识 + 月龄专属福利拉群, 建立专业信任",
			"按月龄段精准推送喂养/护理干货 + 生日关怀",
			"用唤醒券 + 储值锁定高客单耐用品复购",
			"宝妈口碑裂变: 同龄宝妈互相引荐入群",
		},
		Fresh: {
			"以『今天吃什么』每日鲜品播报拉群, 强调时效",
			"早市到货实拍 + 晚市清仓拼团, 制造紧迫感",
			"雨天配送 + 晚间拼团清当日鲜品, 控损耗",
			"邻里拼团自提, 一人下单带动整栋楼入群",
		},
		Apparel: {
			"以换季上新 + 会员专属折扣拉群, 强调『群里先看新款』",
			"上新剧透 + 穿搭内容, 高价值会员专属预告",
			"用会员券 + 换季满减清库存, 守毛利底线",
			"晒单返现 + 闺蜜同行券, 老客带新客试衣",
		},
	}
	if f, ok := focus[business]; ok && stage >= 0 && stage < 4 {
		return f[stage]
	}
	return base[stage]
}

// buildPlaybook 组装一个业态的完整 Playbook.
func buildPlaybook(business string) *Playbook {
	p := paramsFor(business)
	return &Playbook{
		Business:     business,
		Rules:        rulesFor(business),
		Prompts:      promptsFor(business),
		SOP:          sopFor(business),
		LossRate:     p.lossRate,
		CashCoef:     p.cashCoef,
		ColdPriority: append([]string(nil), p.coldPriority...),
	}
}
