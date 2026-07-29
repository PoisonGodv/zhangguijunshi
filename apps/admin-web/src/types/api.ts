// src/types/api.ts — 后端统一响应信封 + 各域 boss 接口响应类型骨架 (FE-ADMIN-01)
//
// 字段命名约定 (经 backend 源码核实, 见 CONTRACT §8-§15 + pkg/errcode):
//  PascalCase (Go 结构体无 json tag, 序列化为字段名本身):
//    catalog.Product / billing.Order,OrderItem,DailySummary,TopProduct / inventory.Batch,Movement /
//    membership.Member,BalanceAccount,RedeemResult,RechargeResult / marketing.Coupon,MemberCoupon /
//    reach.ReachTask,SubscribeAuth,UnsubscribeRecord / groupbuy.Groupbuy,Participant /
//    crm.CommunityGroup,LivingCode / payment.SubMerchant,Payment,PaymentLedger / subscription.Subscription
//  snake_case (每个字段带 json tag):
//    aiengine 栈 (strategy.Strategy,StrategyItem / output.Advice / exec.ExecResult) +
//    attribution DTO + handler 内联 gin.H 包装器 (items/count/code/ok/payment/prepay 等)。
//  例外 (嵌套结构体或单字段带 tag, 不影响外层 PascalCase):
//    Order.PaymentRef→payment_ref; BalanceAccount.RegionCapCents→region_cap_cents;
//    Groupbuy.StrategyID→strategy_id; marketing.CouponParams / groupbuy.GroupbuyParams 整体 snake_case。
//  请求体统一 snake_case (handler 请求结构体均带 json tag)。
//
// 注: 本文件为骨架, A2 agent 填充真实 ProTable/表单时按需补全字段。

// === 统一响应信封 (后端 pkg/httpx Envelope) ===
export interface Envelope<T> {
  code: number
  message: string
  data: T
  trace_id?: string
}

// ============================================================================
// catalog 域 (PascalCase) — CONTRACT §8
// ============================================================================

/** 商品 (catalog.Product, PascalCase, 无 json tag) */
export interface Product {
  ID: number
  TenantID: number
  StoreID: number
  Barcode: string
  Name: string
  Category: string
  Unit: string
  CostCents: number        // 分
  PriceCents: number       // 分
  ImageURL: string
  ShelfLifeDays: number
  Tags: string[]
  Status: string           // active|archived
  CreatedAt: string
  UpdatedAt: string
}

/** ListProducts 响应 (handler 内联 gin.H 包装器, snake_case 键) */
export interface ProductListResp {
  items: Product[]
  count: number
}

// ============================================================================
// billing 域 (PascalCase) — CONTRACT §8
// ============================================================================

/** 订单 (billing.Order, PascalCase; 例外 PaymentRef→payment_ref) */
export interface Order {
  ID: number
  TenantID: number
  StoreID: number
  MemberID: number
  OrderNo: string
  Channel: string          // in_store|wxshop|wework|delivery
  Status: string           // pending|paid|refunded|voided
  TotalCents: number
  PaidCents: number
  DiscountCents: number
  PaymentMethod: string
  OperatorID: number
  Items: OrderItem[]
  PaidAt: string | null
  CreatedAt: string
  UpdatedAt: string
  payment_ref?: string     // 例外: json tag
}

export interface OrderItem {
  ProductID: number
  NameSnapshot: string
  Unit: string
  Qty: number
  PriceCents: number
  CostCents: number
}

/** CreateOrder/CreatePendingOrder 请求体 (snake_case) */
export interface CreateOrderReq {
  store_id: number
  member_id: number
  channel: string
  payment_method: string
  items: { product_id: number; qty: number }[]
}

/** 当日流水摘要 (billing.DailySummary, PascalCase) */
export interface DailySummary {
  TenantID: number
  StoreID: number
  Date: string             // YYYY-MM-DD
  OrderCount: number
  GrossCents: number
  NetCents: number
  CostCents: number
  ProfitCents: number
  TopProducts: TopProduct[]
}

export interface TopProduct {
  ProductID: number
  Name: string
  Qty: number
  GrossCents: number
}

// ============================================================================
// inventory 域 (PascalCase) — CONTRACT §9
// ============================================================================

/** 库存批次 (inventory.Batch, PascalCase) */
export interface Batch {
  ID: number
  TenantID: number
  StoreID: number
  ProductID: number
  Quantity: number
  InPriceCents: number
  ExpiryDate: string | null
  ExpiryWarnDate: string | null
  Status: string           // active|nearing|expired|cleared
  CreatedAt: string
  UpdatedAt: string
}

/** 库存流水 (inventory.Movement, PascalCase) */
export interface Movement {
  ID: number
  TenantID: number
  StoreID: number
  ProductID: number
  BatchID: number
  Type: string             // purchase_in|sale_out|stocktake_adj|return_in|loss|transfer
  Quantity: number
  RefOrderID: number
  OperatorID: number
  Note: string
  CreatedAt: string
}

// ============================================================================
// membership 域 (PascalCase) — CONTRACT §9
// ============================================================================

/** 会员 (membership.Member, PascalCase) */
export interface Member {
  ID: number
  TenantID: number
  StoreID: number
  PhoneHash: string
  WxOpenID: string
  Name: string
  Level: string            // normal|vip|blackcard
  Points: number
  TotalSpentCents: number
  LastVisitAt: string | null
  Birthday: string | null
  Status: string           // active|blocked
  CreatedAt: string
  UpdatedAt: string
}

/** 储值账户 (membership.BalanceAccount, PascalCase; 例外 RegionCapCents→region_cap_cents) */
export interface BalanceAccount {
  ID: number
  TenantID: number
  MemberID: number
  BalanceCents: number
  GiftCents: number
  region_cap_cents?: number // 例外: json tag
  CreatedAt: string
  UpdatedAt: string
}

export interface RedeemResult {
  MemberID: number
  Points: number
  BalanceCents: number
  ConsumedCents: number
}

export interface RechargeResult {
  MemberID: number
  BalanceCents: number
  GiftCents: number
  RechargedCents: number
  GiftedCents: number
}

// ============================================================================
// marketing 域 (PascalCase 外层; CouponParams 嵌套 snake_case) — CONTRACT §10
// ============================================================================

/** 券 (marketing.Coupon, PascalCase) */
export interface Coupon {
  ID: number
  TenantID: number
  StoreID: number
  Name: string
  Type: string             // full_reduction|discount|cash|gift
  Params: CouponParams     // 嵌套 snake_case
  ValidFrom: string
  ValidTo: string
  TotalQuota: number       // 0=不限
  Issued: number
  Status: string           // draft|active|paused|ended
  CreatedBy: number
  CreatedAt: string
  UpdatedAt: string
}

/** CouponParams (嵌套, snake_case json tag) */
export interface CouponParams {
  threshold_cents?: number
  off_cents?: number
  discount_permille?: number  // 0-1000
  gift_product_id?: number
}

/** 会员券 (marketing.MemberCoupon, PascalCase) */
export interface MemberCoupon {
  ID: number
  TenantID: number
  MemberID: number
  CouponID: number
  Status: string           // unused|used|expired|revoked
  Source: string           // ai_strategy|manual|groupbuy|newmember
  StrategyID: number
  IssuedAt: string
  UsedAt: string | null
  ExpireAt: string | null
  RefOrderID: number
}

// ============================================================================
// reach 域 (PascalCase) — CONTRACT §11
// ============================================================================

/** 触达任务 (reach.ReachTask, PascalCase; blocked 也 200, 走 status/blocked_reason) */
export interface ReachTask {
  ID: number
  TenantID: number
  StoreID: number
  StrategyID: number
  Channel: string          // wework_group|wework_1v1|subscribe_msg|sms
  TemplateCode: string
  TargetCount: number
  Status: string           // pending|sent|blocked|failed
  BlockedReason: string    // freq_circuit|content_filter|unsubscribed|subscribe_quota|risk
  ScheduledAt: string
  SentAt: string
  CreatedAt: string
}

// ============================================================================
// groupbuy 域 (PascalCase 外层; GroupbuyParams 嵌套 snake_case; StrategyID 例外) — CONTRACT §11
// ============================================================================

/** 拼团 (groupbuy.Groupbuy, PascalCase; 例外 StrategyID→strategy_id) */
export interface Groupbuy {
  ID: number
  TenantID: number
  StoreID: number
  ProductID: number
  strategy_id?: number     // 例外: json tag
  Title: string
  Params: GroupbuyParams
  RequiredCount: number
  Joined: number
  Status: string           // active|success|failed|cancelled
  StartAt: string
  EndAt: string
  CreatedAt: string
  UpdatedAt: string
}

export interface GroupbuyParams {
  group_size?: number
  price_cents?: number
  valid_hours?: number
}

// ============================================================================
// aiengine 域 (snake_case, 每字段带 json tag) — CONTRACT §12/§14
// ============================================================================

/** 今日3件事建议 (output.Advice, snake_case; degrade 非空=降级, 仍 200) */
export interface Advice {
  strategy: Strategy
  degrade?: string         // budget|llm|no_provider|cold_start|cache_hit; 空=live_llm
}

/** 策略 (strategy.Strategy, snake_case) */
export interface Strategy {
  id: number
  tenant_id: number
  store_id: number
  for_date: string
  signals: unknown         // json.RawMessage
  data_quality_score: number
  generated_by: string     // cache_hit|offline_pregen|live_llm|rule_only
  llm_tier?: string
  total_cost_microcents: number
  status: string           // generated|partially_executed|executed|dismissed
  items: StrategyItem[]
  created_at: string
}

export interface StrategyItem {
  id: number
  tenant_id: number
  strategy_id: number
  rank: number             // 1-3
  action_type: string      // group_seckill|coupon_wake|moments_post|price_adj|groupbuy|recharge_promo
  reason_type: string
  target?: Record<string, unknown>
  product_id?: number
  params: Record<string, unknown>
  copy: string
  reason?: string
  expected_effect?: Record<string, unknown>
  margin_ok: boolean
  execution_status: string // pending|executed|skipped|failed|blocked
  executed_at?: string
}

/** 一键执行结果 (exec.ExecResult, snake_case; blocked 也 200, 走 status/reason) */
export interface ExecResult {
  strategy_item_id: number
  status: string            // executed|blocked|failed
  reason?: string           // margin_floor|freq_circuit|content_filter|unsubscribed|subscribe_quota|confirm_required|not_executable|not_configured|send_failed
  task_id?: number
  ref_id?: number
}

/** 归因回填 (attributionResponseDTO, snake_case, §14) */
export interface Attribution {
  strategy_id: number
  for_date: string
  items: AttributionItem[]
}

export interface AttributionItem {
  rank: number
  action_type: string
  reason_type: string
  coupon_redeem?: number | null
  visit_uplift?: number | null
  groupbuy_success?: number | null
  repurchase?: number | null
  revenue_cny?: number | null
}

// ============================================================================
// payment / 财务 域 (PascalCase) — CONTRACT §15
// ============================================================================

/** 特约商户 (payment.SubMerchant, PascalCase) */
export interface SubMerchant {
  ID: number
  TenantID: number
  StoreID: number
  Provider: string         // wxpay|alipay
  SubMchID: string
  ApplymentID: string
  Status: string           // pending|reviewing|active|rejected|frozen
  RateBPS: number
  Materials: Record<string, unknown>
  AppliedAt: string
  ApprovedAt: string | null
  UpdatedAt: string
}

/** 支付单 (payment.Payment, PascalCase) */
export interface Payment {
  ID: number
  TenantID: number
  StoreID: number
  SubMerchantID: number
  OutTradeNo: string
  Provider: string
  ProviderTxID: string
  Subject: string           // order|recharge|subscription
  RefID: number
  AmountCents: number
  Type: string              // pay|refund
  Status: string            // pending|success|failed|refunded
  PaidAt: string | null
  CreatedAt: string
  UpdatedAt: string
}

/** 账本流水 (payment.PaymentLedger, PascalCase) */
export interface PaymentLedger {
  ID: number
  TenantID: number
  StoreID: number
  SubMerchantID: number
  OrderID: number
  Provider: string
  ProviderTxID: string
  AmountCents: number
  FeeCents: number
  Type: string
  Status: string
  RawPayload: string       // []byte → base64/字符串
  PaidAt: string | null
  CreatedAt: string
}

/** 订阅 (subscription.Subscription, PascalCase) */
export interface Subscription {
  ID: number
  TenantID: number
  Plan: string              // free|pro|flagship
  PeriodStartedAt: string
  PeriodEndsAt: string | null // nil=永久(仅 free)
  Status: string            // active|expired|cancelled
  Source: string            // manual|wxpay|alipay|admin
  PaymentRef: string
  CreatedAt: string
  UpdatedAt: string
}

/** whoami (GET /v1/boss/whoami, handler gin.H, snake_case 键) */
export interface Whoami {
  tenant_id: number
  source: string
}
