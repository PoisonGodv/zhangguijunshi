// types/api.d.ts — C 端共享类型 (FE-MP-01)
//
// 骨架镜像,后端 C 端 DTO 就绪后校准。中文注释镜像后端 json tag。

/** 统一响应信封 (后端 httpx.OK/Fail: {code,message,data,trace_id}) */
export interface Envelope<T> { code: number; message: string; data: T; trace_id?: string }

/** wx.login 登录返回 (POST /v1/wxshop/auth/login) */
export interface WxLoginResp { access_token: string; refresh_token: string; expires_in?: number }

/** 当前会员身份 (GET /v1/wxshop/auth/me) */
export interface MeResp { tenant_id: number; store_id: number; subject: string; scene?: string; role?: string }

/** 会员信息 (GET /v1/wxshop/member/profile; phone_masked 后端恒空串,前端本地回填) */
export interface MemberInfo {
  member_id: number
  phone_masked: string        // 后端恒 ''(只存 hash); join 页本地回填掩码
  level: string               // normal|vip|blackcard
  level_name: string          // 普通会员/银卡会员/金卡会员
  points: number
  balance_cents: number       // 储值余额(分); 后端暂恒 0 (CONTRACT §16.4 TODO)
  total_spent_cents: number   // 累计消费(分)
  tenant_id: number
  store_id: number
}

/** 会员码 (POST /v1/wxshop/member/code; 60s 一次性 nonce) */
export interface MemberCode { code: string; expire_seconds: number }

/** 券 */
export interface Coupon {
  id: number
  title: string               // 满50减10
  type: 'cash' | 'discount' | 'gift'
  value_text: string          // ¥10 / 8折
  threshold_text?: string     // 满¥50可用
  scope_text?: string         // 全场/指定商品
  expire_text: string         // 2026-08-31 到期
  status: 'unused' | 'used' | 'expired'
}

/** 首页今日福利 */
export interface HomeBenefit { id: number; tag: string; title: string; desc: string; cover?: string }

/** 拼团简报 (首页入口) */
export interface GroupBuyBrief { id: number; title: string; price_cents: number; original_cents: number; required: number; joined: number }

/** 拼团详情 (GET /v1/wxshop/groupbuys/:id; CONTRACT §16.3(8)) */
export interface GroupBuyDetail {
  id: number
  title: string
  desc: string               // C 端文案 (本期后端 = title, TODO 独立 desc)
  price_cents: number        // 团购价(分)
  original_cents: number    // 原价(分; 后端无原价字段,降级=团购价)
  required: number           // 成团门槛
  joined: number             // 权威参团人数(ListParticipants 计数)
  status: string             // active|success|failed|cancelled
  expire: string             // 截止时间 ISO (EndAt)
}

/** 门店 */
export interface Store {
  id: number
  name: string
  address: string
  distance_text?: string
  phone?: string            // 门店电话 (B1 storeDTO 补)
  group_qr?: string         // 进群二维码 (文本/图片URL)
  active?: boolean          // 门店启用状态 (B1 storeDTO 对齐; 列表已过滤 active)
}

/** 储值余额 (GET /v1/wxshop/member/balance; 本金 + 赠送) */
export interface Balance {
  balance_cents: number   // 本金(分)
  gift_cents: number      // 赠送(分)
}

/** 储值充值结果 (POST /v1/wxshop/member/balance/recharge; CONTRACT §24.4)
 *  PascalCase: 镜像后端 membership.RechargeResult (membership 域实体无 json tag, 与 boss 端 /membership/members/:id/recharge 同形)
 *  BalanceCents/GiftCents = 充值后余额; RechargedCents/GiftedCents = 本次入账 */
export interface RechargeResult {
  MemberID: number
  BalanceCents: number
  GiftCents: number
  RechargedCents: number
  GiftedCents: number
}

/** 订单明细行 (GET /v1/wxshop/orders → items[]; CONTRACT §16.3(7)) */
export interface OrderItem {
  name: string
  unit: string            // 件 / 瓶 / 斤 ...
  qty: number
  price_cents: number
}

/** 订单行 (GET /v1/wxshop/orders; CONTRACT §16.3(7)) */
export interface Order {
  id: number
  order_no: string
  status: 'pending' | 'paid' | 'refunded' | 'voided'
  total_cents: number
  paid_cents: number
  created_at: string     // ISO 时间
  items: OrderItem[]
}

/** 订单详情 (GET /v1/wxshop/orders/:id; CONTRACT §24.3)
 *  snake_case; 复用 OrderItem; channel/payment_method/paid_at omitempty (后端 toOrderDTO 已剥敏感字段) */
export interface OrderDetail {
  id: number
  order_no: string
  status: 'pending' | 'paid' | 'refunded' | 'voided'
  total_cents: number
  paid_cents: number
  channel?: string          // 支付渠道 (线下/微信/...; pending 订单可能缺省)
  payment_method?: string   // 支付方式 (余额/现金/...; pending 订单可能缺省)
  created_at: string        // ISO 时间
  paid_at?: string          // ISO 时间 (paid/refunded 才有; pending/voided 缺省)
  items: OrderItem[]
}

/** 积分流水 (GET /v1/wxshop/member/points → logs[]; CONTRACT §16.3(10)) */
export interface PointLog {
  type: string             // earn|redeem|expire|adjust
  delta: number            // 正负
  points_after: number
  ref_order_id?: number     // 关联订单 (后端 omitempty; 无关联则不出)
  created_at: string       // ISO 8601
}

/** 积分明细结果 (GET /v1/wxshop/member/points; CONTRACT §16.3(10))
 *  points: 会员当前积分(权威运行余额); logs: 流水(倒序, limit 默认 50) */
export interface PointsResult {
  points: number
  logs: PointLog[]
}

/** 券详情 (GET /v1/wxshop/coupons/:id; CONTRACT §16.3(9))
 *  detail: 使用规则 + 有效期; available: 调用方是否仍可领取/使用 */
export interface CouponDetail extends Coupon {
  detail: {
    threshold_text?: string  // 满¥N可用 / 无门槛
    scope_text?: string       // 全场通用 / 指定商品
    value_text?: string       // ¥N / N折 / 赠品
    valid_from?: string       // ISO date
    valid_to?: string         // ISO date
  }
  available: boolean           // true = 调用方可领/可用
  expire: string               // ISO date 或 "" 无
}
