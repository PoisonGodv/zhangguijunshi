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

/** 门店 */
export interface Store { id: number; name: string; address: string; distance_text?: string; group_qr?: string }
