// src/services/membership.ts — 会员域 boss 接口封装 (FE-ADMIN-02)
//
// 对齐 backend/interfaces/http/handler/membership.go + internal/membership/membership.go:
//   - memberDTO (handler 内联结构体, 每字段带 json tag) → snake_case:
//     id/level/level_name/name?/points/balance_cents/total_spent_cents/created_at/block。
//     刻意隐藏 phone_hash/wx_openid (敏感 PII / 幂等键)。
//   - ListMembers 响应 gin.H{"items":..,"total":..} (snake_case 键; total=真总量,
//     后端真分页 page/size)。
//   - RechargeResult / RedeemResult 是 membership 域结构体无 json tag → PascalCase。
//   - IssueCode 响应 gin.H{"code":..,"member_id":..} (snake_case)。

import { api } from '@/services/request'

/** 会员 DTO (handler memberDTO, snake_case; boss/admin 可见字段) */
export interface MemberDTO {
  id: number
  level: string // normal | vip | blackcard
  level_name: string
  name?: string
  points: number
  balance_cents: number
  total_spent_cents: number
  created_at: string
  block: boolean
}

export interface ListMembersParams {
  page: number // >=1
  size: number // <=200, 默认 20
  level?: string // normal | vip | blackcard
  q?: string // 名称/手机号 hash 模糊
}

/** ListMembers 响应 (gin.H, snake_case 键; total=真总量) */
export interface ListMembersResp {
  items: MemberDTO[]
  total: number
}

/** 储值充值请求体 (handler rechargeRequest, snake_case; amount_cents 必填>=1) */
export interface RechargeBody {
  amount_cents: number
}

/** 储值充值结果 (membership.RechargeResult, PascalCase) */
export interface RechargeResult {
  MemberID: number
  BalanceCents: number
  GiftCents: number
  RechargedCents: number
  GiftedCents: number
}

/** 会员码核销请求体 (handler redeemRequest, snake_case; code 必填) */
export interface RedeemBody {
  store_id?: number
  code: string
  consume_cents?: number
  earn_points?: number
  ref_order_id?: number
}

/** 会员码核销结果 (membership.RedeemResult, PascalCase) */
export interface RedeemResult {
  MemberID: number
  Points: number
  BalanceCents: number
  ConsumedCents: number
}

/** 下发会员码结果 (handler gin.H, snake_case) */
export interface IssueCodeResult {
  code: string
  member_id: number
}

/** GET /v1/boss/membership/members?page=&size=&level=&q= — 会员分页列表 (后端真分页) */
export function listMembers(params: ListMembersParams): Promise<ListMembersResp> {
  return api.get<ListMembersResp>('/boss/membership/members', { params })
}

/** GET /v1/boss/membership/members/:id — 会员详情 (memberDTO) */
export function getMember(id: number): Promise<MemberDTO> {
  return api.get<MemberDTO>(`/boss/membership/members/${id}`)
}

/** POST /v1/boss/membership/members/:id/recharge — 储值充值 (反二清: 仅记账, 资金直清子商) */
export function recharge(id: number, body: RechargeBody): Promise<RechargeResult> {
  return api.post<RechargeResult>(`/boss/membership/members/${id}/recharge`, body)
}

/** POST /v1/boss/membership/redeem — 会员码核销 (扣储值/累积分, 原子) */
export function redeem(body: RedeemBody): Promise<RedeemResult> {
  return api.post<RedeemResult>('/boss/membership/redeem', body)
}

/** POST /v1/boss/membership/members/:id/code — 下发动态会员码 */
export function issueCode(id: number): Promise<IssueCodeResult> {
  return api.post<IssueCodeResult>(`/boss/membership/members/${id}/code`)
}
