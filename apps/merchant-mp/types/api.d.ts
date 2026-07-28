// types/api.d.ts — TS 类型镜像后端 CONTRACT.md §12 json tag
//
// 注意: TypeScript 规范下 .d.ts 声明文件不能包含运行时常量初值 (TS1039),
// 故中文映射常量 (ACTION_TYPE_LABEL / REASON_LABEL / DEGRADE_HINT) 定义在各页面 .ts 顶部
// (today.ts / task-detail.ts),此处仅声明与后端 json tag 一一对应的接口类型。

/** 统一响应信封 (CONTRACT §12.7 / 技术方案 §9.2.5: {code,message,data,trace_id}) */
export interface Envelope<T> {
  code: number
  message: string
  data: T
  trace_id?: string
}

/** 营销策略 (CONTRACT §12.3.1 strategy.Strategy —— 严格镜像 json tag) */
export interface Strategy {
  id: number
  tenant_id: number
  store_id: number
  for_date: string // YYYY-MM-DD
  signals?: Record<string, any> // 当日 signal 快照 (jsonb)
  data_quality_score?: number // 0-100
  generated_by: string // cache_hit|offline_pregen|live_llm|rule_only
  llm_tier?: string // rule_only 时空
  total_cost_microcents: number // 1元=10000微分
  status: string // generated|partially_executed|executed|dismissed
  items: StrategyItem[]
  created_at: string
}

/** 策略条目 (CONTRACT §12.3.1 strategy.StrategyItem —— 严格镜像 json tag) */
export interface StrategyItem {
  id: number
  tenant_id?: number
  strategy_id: number
  rank: number // 1-3
  action_type: string // group_seckill|coupon_wake|moments_post|price_adj|groupbuy|recharge_promo
  reason_type: string // 7 值语义 (clearance_coupon|rain_delivery|bundle_slow|wake_dormant|festival_discount|birthday_care|high_value_push)
  target: Record<string, any> // 受众筛选 (jsonb)
  product_id: number // 0=非商品绑定
  params: Record<string, any> // 折扣/满减/文案 (jsonb)
  copy: string // ≤40 字
  reason: string // 老板可读推荐理由
  expected_effect: Record<string, any> | string // 预期效果 (jsonb,历史字段可能是 string,前端两者兼容)
  margin_ok: boolean // 毛利护栏校验
  execution_status: string // pending|executed|skipped|failed|blocked
  executed_at?: string
}

/** 今日军师建议 (CONTRACT §12.3.7 output.Advice) */
export interface Advice {
  strategy: Strategy
  degrade?: string // budget|llm|no_provider|cold_start|cache_hit (空=live_llm)
}

/** 一键执行结果 (CONTRACT §12.3.9 exec.ExecResult —— 严格镜像 json tag) */
export interface ExecResult {
  strategy_item_id: number
  status: 'executed' | 'blocked' | 'failed'
  reason?: string // margin_floor|freq_circuit|content_filter|unsubscribed|subscribe_quota|confirm_required|not_executable|not_configured
  task_id?: number // reach.ReachTask.ID (触达类)
  ref_id?: number // MemberCouponID / GroupbuyID (按 ActionType 语义)
}

/** 执行入参 (CONTRACT §12.3.9 ExecuteInput: {confirm?}) */
export interface ExecuteBody {
  confirm?: boolean // price_adj 高危动作需老板二次确认,前端统一传 true
}

/** 「今日3件事」首页卡片视图模型 (today.ts 内部用) */
export interface AdviceCardVM {
  id: number
  strategyId: number
  rank: number
  actionLabel: string
  actionTagClass: string
  copy: string
  reason: string
  effect: string
  marginOk: boolean
}

/** 建议详情视图模型 (task-detail.ts 内部用) */
export interface DetailVM {
  itemId: number
  strategyId: number
  actionLabel: string
  reason: string
  copy: string
  paramsText: string
  targetText: string
  effectText: string
  marginOk: boolean
  actionType: string
  executed: boolean
}
