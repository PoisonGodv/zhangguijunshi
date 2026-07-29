// src/services/marketing.ts — 营销券 boss 接口封装 (FE-ADMIN-02)
//
// 后端 CONTRACT §10.3: /v1/boss/marketing/* 券模板 + 会员券。
// marketing.Coupon 外层 PascalCase (无 json tag), CouponParams 嵌套 snake_case
// (有 json tag), MemberCoupon 外层 PascalCase (无 json tag)。本文件类型按实对齐。
//
// 状态机: Coupon.Status draft → active ↔ paused → ended;
//         MemberCoupon.Status unused → used|expired|revoked。
// 毛利护栏: POST /coupons/issue 可能返 60001 (ErrMarginGuard), 前端按 errcode 映射 toast。
//
// 注: /coupons/:id/status 是 PATCH 路由, request.ts 的 api 仅暴露 get/post/put/del,
// 故此处复用 default 导出的 axios 实例走 client.patch (不改 request.ts)。

import client, { api } from './request'

/** 本域 PATCH 便捷封装 (与 api 风格一致: 解 envelope 取 data) */
function patch<T>(url: string, data: unknown): Promise<T> {
  return client.patch<unknown, T>(url, data)
}

/** 券参数 (嵌套, snake_case; 按 Type 选取不同字段) */
export interface CouponParams {
  threshold_cents?: number // full_reduction: 满 threshold 减 off
  off_cents?: number // full_reduction/cash: 减/代金
  discount_permille?: number // discount: 0-1000 (850=85%=8.5折)
  gift_product_id?: number // gift: 赠品 ID
}

/** 券模板 (marketing.Coupon, 外层 PascalCase) */
export interface Coupon {
  ID: number
  TenantID: number
  StoreID: number
  Name: string
  Type: string // full_reduction|discount|cash|gift
  Params: CouponParams
  ValidFrom: string
  ValidTo: string
  TotalQuota: number // 0=不限发行
  Issued: number // 已发数量
  Status: string // draft|active|paused|ended
  CreatedBy: number
  CreatedAt: string
  UpdatedAt: string
}

/** 会员券实例 (marketing.MemberCoupon, 外层 PascalCase) */
export interface MemberCoupon {
  ID: number
  TenantID: number
  MemberID: number
  CouponID: number
  Status: string // unused|used|expired|revoked
  Source: string // ai_strategy|manual|groupbuy|newmember
  StrategyID: number // 归因 (0=非AI来源)
  IssuedAt: string
  UsedAt: string | null
  ExpireAt: string | null
  RefOrderID: number
}

/** 列表过滤参数 (handler Query) */
export interface CouponListParams {
  status?: string
  type?: string
  store_id?: number
  limit?: number
  offset?: number
}

/** 创建券请求体 (snake_case, 对齐 createCouponRequest) */
export interface CreateCouponBody {
  store_id?: number
  name: string
  type: string // full_reduction|discount|cash|gift
  params: CouponParams
  valid_from?: string // ISO, 可空 (后端 ValidFrom 零值=创建时刻)
  valid_to: string // ISO, 必填
  total_quota?: number // 0=不限
}

/** 改状态请求体 */
export interface UpdateStatusBody {
  status: string // active|paused|ended 等
}

/** 发券请求体 (snake_case, 对齐 issueRequest) */
export interface IssueCouponBody {
  member_id: number
  coupon_id: number
  source?: string // 空=manual
  strategy_id?: number
  expire_at?: string // ISO, 空=用 coupon.ValidTo
}

/** 核销请求体 (snake_case, 对齐 redeemCouponRequest) */
export interface RedeemBody {
  member_coupon_id: number
  ref_order_id?: number
}

/** GET /v1/boss/marketing/coupons — 券模板列表 */
export function listCoupons(params?: CouponListParams): Promise<Coupon[]> {
  return api.get<Coupon[]>('/boss/marketing/coupons', { params })
}

/** POST /v1/boss/marketing/coupons — 创建券模板 (draft) */
export function createCoupon(body: CreateCouponBody): Promise<Coupon> {
  return api.post<Coupon>('/boss/marketing/coupons', body)
}

/** GET /v1/boss/marketing/coupons/:id — 券详情 */
export function getCoupon(id: number): Promise<Coupon> {
  return api.get<Coupon>(`/boss/marketing/coupons/${id}`)
}

/** PATCH /v1/boss/marketing/coupons/:id/status — 上线/下线/暂停/终止 */
export function updateStatus(id: number, status: string): Promise<Coupon> {
  return patch<Coupon>(`/boss/marketing/coupons/${id}/status`, { status } as UpdateStatusBody)
}

/** POST /v1/boss/marketing/coupons/issue — 给会员发券 (可能返 60001 毛利护栏) */
export function issueCoupon(body: IssueCouponBody): Promise<MemberCoupon> {
  return api.post<MemberCoupon>('/boss/marketing/coupons/issue', body)
}

/** POST /v1/boss/marketing/coupons/redeem — 核销会员券 */
export function redeemCoupon(body: RedeemBody): Promise<MemberCoupon> {
  return api.post<MemberCoupon>('/boss/marketing/coupons/redeem', body)
}

/** GET /v1/boss/marketing/members/:id/coupons — 会员券列表 */
export function listMemberCoupons(memberId: number, status?: string): Promise<MemberCoupon[]> {
  return api.get<MemberCoupon[]>(`/boss/marketing/members/${memberId}/coupons`, {
    params: status ? { status } : undefined,
  })
}
