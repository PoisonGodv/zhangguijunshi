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

/**
 * 登录/续期响应 (POST /v1/boss/auth/login 与 POST /v1/wxshop/auth/refresh 同构,CONTRACT §19)
 * refresh 走 X-Refresh-Token 头 (首选) 或 body {refresh_token}。
 */
export interface LoginResp {
  access_token: string
  refresh_token: string
  expires_in?: number
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

// ============================================================
// === 商品 / 订单 (billing 开单页) ===
//
// 后端字段命名警告: catalog.Product / billing.Order 结构体**未声明 json tag**,
// Go encoding/json 默认按字段名 (PascalCase) 输出,与 interfaces/http/*_test.go 断言一致。
// 因此下方接口使用 PascalCase (ID/Name/PriceCents/...),而非本项目其他类型的 snake_case。
// CreateOrderReq/CreateOrderItem 是 handler 的 DTO,有显式 json tag,走 snake_case。
// ============================================================

/** 商品 (catalog.Product — 无 json tag, PascalCase 输出) */
export interface Product {
  ID: number
  TenantID: number
  StoreID: number
  Barcode: string
  Name: string
  Category: string
  Unit: string
  CostCents: number
  PriceCents: number
  ImageURL: string
  ShelfLifeDays: number
  Tags?: string[]
  Status: string // "active" | "archived"
  CreatedAt: string
  UpdatedAt: string
}

/** 商品列表响应 (handler.ListProducts 返回 gin.H{items, count}) */
export interface ProductListResp {
  items: Product[]
  count: number
}

/** 开单行 (handler.createOrderItemRequest json tag = snake_case) */
export interface CreateOrderItem {
  product_id: number
  qty: number
}

/** 开单请求体 (handler.createOrderRequest; CreatePendingOrder 与 CreateOrder 同构) */
export interface CreateOrderReq {
  store_id?: number // 缺省时后端用 ctx.StoreID
  member_id?: number // 散客=0
  channel?: string // 默认 "in_store"
  payment_method?: string // 默认 "cash"
  items: CreateOrderItem[] // 至少 1 行
}

/** 订单行 (billing.OrderItem — 无 json tag, PascalCase) */
export interface OrderItem {
  ProductID: number
  NameSnapshot: string
  Unit: string
  Qty: number
  PriceCents: number
  CostCents: number
}

/** 订单 (billing.Order — 无 json tag, PascalCase) */
export interface Order {
  ID: number
  TenantID: number
  StoreID: number
  MemberID: number
  OrderNo: string
  Channel: string // in_store|wxshop|wework|delivery
  Status: string // pending|paid|refunded|voided
  TotalCents: number
  PaidCents: number
  DiscountCents: number
  PaymentMethod: string
  OperatorID: number
  Items: OrderItem[]
  PaidAt?: string // ISO 时间, paid 后填
  CreatedAt: string
  UpdatedAt: string
  PaymentRef?: string
}

// === me 我的页 (FE-MP-04) —— /v1/boss 下经营/订阅/门店数据 ===

/**
 * 当日流水摘要 (GET /v1/boss/billing/summary/today)
 * 镜像后端 billing.DailySummary (internal/billing/billing.go),字段为 PascalCase
 * (后端未加 json tag,Go encoding/json 默认序列化字段名)。
 */
export interface DailySummary {
  TenantID: number
  StoreID: number
  Date: string // YYYY-MM-DD
  OrderCount: number
  GrossCents: number // Σ total (分)
  NetCents: number   // Σ paid (分)
  CostCents: number  // Σ 各项 qty*cost (分)
  ProfitCents: number // Net - Cost (分)
  TopProducts?: TopProduct[] | null
}

/** 热销商品 (billing.TopProduct) */
export interface TopProduct {
  ProductID: number
  Name: string
  Qty: number
  GrossCents: number
}

/**
 * 订阅记录 (subscription.Subscription, internal/subscription/subscription.go)
 * 无 json tag → PascalCase;PeriodEndsAt 为 *time.Time,永久 free 时为 null。
 */
export interface Subscription {
  ID: number
  TenantID: number
  Plan: string // free|pro|flagship
  PeriodStartedAt: string
  PeriodEndsAt?: string | null // nil=永久 (仅 free)
  Status: string // active|expired|cancelled
  Source: string // manual|wxpay|alipay|admin
  PaymentRef?: string
  CreatedAt: string
  UpdatedAt: string
}

/**
 * GET /v1/boss/subscription 响应 (handler/subscription.go GetSubscription):
 *   httpx.OK(c, gin.H{"plan": plan, "subscription": sub})
 * sub 可能为 null (无任何订阅记录,CurrentPlan 默认 free)。
 * §25.2: MVP 砍在线支付/订阅,该路由已下架(404),前端不再调用;类型保留待 V1 恢复。
 */
export interface SubscriptionResp {
  plan: string
  subscription?: Subscription | null
}

/**
 * GET /v1/boss/membership/members?page=&size=&level=&q= 响应 (CONTRACT §17.11):
 *   httpx.OK(c, gin.H{"items": memberDTO[], "total": n})
 * me 页会员数卡片只读 total (page=1&size=1 最小拉取),items 用 unknown[] 不展开 memberDTO;
 * 充值页 (§25.3) 用 items 渲染列表,展开为 MemberDTO。
 */
export interface ListMembersResp {
  items: MemberDTO[]
  total: number
}

/**
 * boss 端会员 DTO (handler/membership.go memberDTO, 有 json tag → snake_case)。
 * 仅暴露 id/等级/积分/余额/累计消费/创建时间/冻结态,刻意隐藏 phone_hash/openid (PII)。
 * balance_cents 为真实储值实付余额 (无账户读 0)。
 */
export interface MemberDTO {
  id: number
  level: string // normal|vip|blackcard
  level_name: string // 普通会员|银卡会员|金卡会员
  name?: string
  points: number
  balance_cents: number
  total_spent_cents: number
  created_at: string
  block: boolean
}

/**
 * 充值结果 (POST /v1/boss/membership/members/:id/recharge, CONTRACT §25.3)。
 * membership.RechargeResult **无 json tag** → Go encoding/json 默认 PascalCase。
 * BalanceCents/GiftCents 为入账后总额; RechargedCents/GiftedCents 为本次增量。
 * 赠送 (GiftedCents) 由后端 GiftForAmount 档位规则算,前端绝不传 gift。
 */
export interface RechargeResult {
  MemberID: number
  BalanceCents: number   // 入账后实付余额 (分)
  GiftCents: number      // 入账后赠送余额 (分)
  RechargedCents: number // 本次实付 (分)
  GiftedCents: number    // 本次赠送 (分)
}

/**
 * GET /v1/boss/whoami 响应 (interfaces/http/router.go whoami):
 *   httpx.OK(c, gin.H{"tenant_id": tid, "source": source})
 * 注: 后端目前只返回 tenant_id + source,**没有** store_id / role / scene。
 */
export interface Whoami {
  tenant_id: number
  source: string // consumer|merchant|admin|system / unresolved
}
