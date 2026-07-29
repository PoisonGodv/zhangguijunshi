// src/services/aiengine.ts — AI 策略 boss 接口封装 (FE-ADMIN-02)
//
// 后端 CONTRACT §12/§14: /v1/boss/aiengine/* 今日3件事 + 策略回放 + 一键执行 + 归因。
// aiengine 全栈 snake_case (每个字段带 json tag), 与 billing/marketing 的 PascalCase 不同。
//
// "200-as-data" 模式 (关键, 前端按字段分支, 不按 errcode):
//   - POST /today 返 Advice { strategy, degrade? }; degrade 非空 = 降级 (budget/llm/
//     no_provider/cold_start/cache_hit), 空 = live_llm; 仍是 200 + code=0。
//   - POST /strategies/:id/items/:itemID/execute 返 ExecResult { status, reason? };
//     status=blocked 时非错 (200 + code=0), 前端按 status 字段分支展示归因。
//   - GET /strategies/:id/attribution 缺失 metric 字段为 null (非 0), 看板显示 "—"。

import { api } from './request'

/** 策略项 (strategy.StrategyItem, snake_case) */
export interface StrategyItem {
  id: number
  tenant_id: number
  strategy_id: number
  rank: number // 1-3
  action_type: string // group_seckill|coupon_wake|moments_post|price_adj|groupbuy|recharge_promo
  reason_type: string
  target?: Record<string, unknown>
  product_id?: number
  params: Record<string, unknown>
  copy: string // boss 可见文案
  reason?: string
  expected_effect?: Record<string, unknown>
  margin_ok: boolean
  execution_status: string // pending|executed|skipped|failed|blocked
  executed_at?: string
}

/** 策略 (strategy.Strategy, snake_case) */
export interface Strategy {
  id: number
  tenant_id: number
  store_id: number
  for_date: string
  signals: unknown // json.RawMessage
  data_quality_score: number
  generated_by: string // cache_hit|offline_pregen|live_llm|rule_only
  llm_tier?: string
  total_cost_microcents: number
  status: string // generated|partially_executed|executed|dismissed
  items: StrategyItem[]
  created_at: string
}

/** 今日3件事建议 (output.Advice, snake_case) */
export interface Advice {
  strategy: Strategy
  degrade?: string // 空=live_llm; budget|llm|no_provider|cold_start|cache_hit
}

/** 一键执行结果 (exec.ExecResult, snake_case; blocked 也 200) */
export interface ExecResult {
  strategy_item_id: number
  status: string // executed|blocked|failed
  reason?: string // margin_floor|freq_circuit|content_filter|...|send_failed|not_executable|not_configured
  task_id?: number // reach 任务 id
  ref_id?: number // 会员券 id / 拼团 id (按 action_type 语义)
}

/** 归因单项 (attributionItemDTO, snake_case; 缺失 metric 为 undefined) */
export interface AttributionItem {
  rank: number
  action_type: string
  reason_type: string
  coupon_redeem?: number | null // 核销率 0-1
  visit_uplift?: number | null // 到店反推增量
  groupbuy_success?: number | null // 拼团成团率 0-1
  repurchase?: number | null // 触达会员复购率 0-1
  revenue_cny?: number | null // 增收估算 (元)
}

/** 归因响应 (attributionResponseDTO, snake_case) */
export interface Attribution {
  strategy_id: number
  for_date: string
  items: AttributionItem[]
}

/** 策略列表过滤参数 (handler Query) */
export interface StrategyListParams {
  date?: string
  status?: string
}

/** POST /v1/boss/aiengine/today — 生成/取缓存今日3件事 (body {date?} 可选) */
export function today(date?: string): Promise<Advice> {
  return api.post<Advice>('/boss/aiengine/today', date ? { date } : {})
}

/** GET /v1/boss/aiengine/strategies — 策略列表 */
export function listStrategies(params?: StrategyListParams): Promise<Strategy[]> {
  return api.get<Strategy[]>('/boss/aiengine/strategies', { params })
}

/** GET /v1/boss/aiengine/strategies/:id — 策略详情 (含 items) */
export function getStrategy(id: number): Promise<Strategy> {
  return api.get<Strategy>(`/boss/aiengine/strategies/${id}`)
}

/** POST /v1/boss/aiengine/strategies/:id/items/:itemID/execute — 一键执行
 *  返 ExecResult { status: executed|blocked|failed }; blocked 也 200, 按 status 分支。 */
export function executeItem(
  strategyId: number,
  itemId: number,
  confirm?: boolean,
): Promise<ExecResult> {
  return api.post<ExecResult>(
    `/boss/aiengine/strategies/${strategyId}/items/${itemId}/execute`,
    confirm === undefined ? {} : { confirm },
  )
}

/** POST /v1/boss/aiengine/strategies/:id/items/:itemID/feedback — 反馈有用 (body {useful}) */
export function recordFeedback(
  strategyId: number,
  itemId: number,
  useful: boolean,
): Promise<{ ok: boolean }> {
  return api.post<{ ok: boolean }>(
    `/boss/aiengine/strategies/${strategyId}/items/${itemId}/feedback`,
    { useful },
  )
}

/** GET /v1/boss/aiengine/strategies/:id/attribution — 归因看板 (缺失 metric 为 null) */
export function getAttribution(id: number): Promise<Attribution> {
  return api.get<Attribution>(`/boss/aiengine/strategies/${id}/attribution`)
}
