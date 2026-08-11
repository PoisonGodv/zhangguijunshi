// services/member.ts — 会员服务 (真实: /v1/wxshop/member/{profile,code,claim-newcomer})
//
// USE_MOCK=false: 接后端 #2/#3/#4。失败时降级回 mock (离线/后端未起兜底)。
// 注意: 后端 MemberCode 返回 {code, expire_seconds:60} (相对秒数,非绝对时间戳);
// 后端 profile.phone_masked 恒空串 (只存 hash), 前端 join 页本地回填掩码。
// 注意: claimNewMemberCoupon 传原始 11 位手机号 (后端自己 hash 存储,前端勿传掩码)。

import { http } from '../utils/request'
import type { MemberInfo, MemberCode, Coupon, Balance, PointsResult, RechargeResult } from '../types/api'

const USE_MOCK = false // 后端 /v1/wxshop/member/* 已就绪 (CONTRACT §16)

let codeSeq = 0

/** 生成动态会员码 (POST /v1/wxshop/member/code) → {code, expire_seconds:60} */
export function genMemberCode(): Promise<MemberCode> {
  if (USE_MOCK) {
    codeSeq += 1
    const nonce = `mock-${Date.now().toString(36)}-${codeSeq}-${Math.random().toString(36).slice(2, 8)}`
    return Promise.resolve({ code: nonce, expire_seconds: 60 })
  }
  return http.post<MemberCode>('/v1/wxshop/member/code')
}

/**
 * 我的会员信息 (GET /v1/wxshop/member/profile) → MemberInfo。
 * 降级: 真接口失败回 mock 占位 (member_id 999, 不再用 auth/me subject 伪造)。
 */
export async function getMyProfile(): Promise<MemberInfo> {  if (USE_MOCK) {
    return {
      member_id: 999,
      phone_masked: '',
      level: 'vip',
      level_name: '银卡会员',
      points: 320 + (codeSeq % 50),
      balance_cents: 6600,
      total_spent_cents: 12800,
      tenant_id: 1,
      store_id: 1,
    }
  }
  try {
    return await http.get<MemberInfo>('/v1/wxshop/member/profile')
  } catch (err) {
    // 降级: 真接口失败 → 回 mock 占位 (离线/后端未起兜底)
    console.warn('[member] getMyProfile 真接口失败,降级 mock', err)
    return {
      member_id: 0,
      phone_masked: '',
      level: 'normal',
      level_name: '普通会员',
      points: 0,
      balance_cents: 0,
      total_spent_cents: 0,
      tenant_id: 0,
      store_id: 0,
    }
  }
}

/**
 * 领取新人券 (POST /v1/wxshop/member/claim-newcomer {phone})。
 * 幂等: 已领返回 {coupon, already_claimed:true}。phone 传原始 11 位 (后端自己 hash)。
 * 降级: 真接口失败回 mock 券 (离线/后端未起兜底)。
 */
export function claimNewMemberCoupon(phone: string): Promise<{ coupon: Coupon; already_claimed?: boolean }> {
  if (USE_MOCK) {
    void phone
    return Promise.resolve({
      coupon: {
        id: 9000 + (codeSeq % 100),
        title: '新人见面礼 满30减10',
        type: 'cash',
        value_text: '¥10',
        threshold_text: '满¥30可用',
        scope_text: '全场通用',
        expire_text: '领取后 7 天内有效',
        status: 'unused',
      },
    })
  }
  return http
    .post<{ coupon: Coupon; already_claimed?: boolean }>('/v1/wxshop/member/claim-newcomer', { phone })
    .catch((err) => {
      // 降级: 真接口失败 → 回 mock 券 (离线/后端未起兜底)
      console.warn('[member] claimNewMemberCoupon 真接口失败,降级 mock', err)
      return {
        coupon: {
          id: 9001,
          title: '新人见面礼 满30减10',
          type: 'cash',
          value_text: '¥10',
          threshold_text: '满¥30可用',
          scope_text: '全场通用',
          expire_text: '领取后 7 天内有效',
          status: 'unused',
        },
      }
    })
}

/**
 * 我的储值余额 (GET /v1/wxshop/member/balance) → {balance_cents, gift_cents}。
 * 后端: membership.GetAccount 真实返; 新会员 (无 account 行) 返 0/0, 不 404。
 * 降级: 真接口失败 → 回 0/0 (页面空态展示, 不阻断)。
 */
export async function getBalance(): Promise<Balance> {
  if (USE_MOCK) {
    return { balance_cents: 6600, gift_cents: 600 }
  }
  try {
    return await http.get<Balance>('/v1/wxshop/member/balance')
  } catch (err) {
    // 降级: 真接口失败 → 回 0/0 (页面展示空余额, 不阻断)
    console.warn('[member] getBalance 真接口失败,降级 0/0', err)
    return { balance_cents: 0, gift_cents: 0 }
  }
}

/**
 * 储值充值 Simulate (POST /v1/wxshop/member/balance/recharge {amount_cents}; CONTRACT §24.4)。
 * dev/test 便利 (对齐 billing.SimulatePay): 直接入账 membership.Recharge, 不走 wx.requestPayment / wxpay。
 * 写操作不降级, 失败直接抛 ApiError (页面 toast 中文):
 *   60003 超过储值上限 (membership.ErrBalanceOver→60003, rechargeAtomic cap 校验);
 *   40404 会员不存在 / active 校验失败。
 * 真实 wxpay 链路 (paymentFulfiller SubjectRecharge→Recharge 闭环已就位 §19) 延后到支付联调;
 * 生产环境必须切真实 wxpay (资金直清 sub_merchant, 反二清硬约束 §15.5.3)。
 */
export function recharge(amountCents: number): Promise<RechargeResult> {
  return http.post<RechargeResult>('/v1/wxshop/member/balance/recharge', { amount_cents: amountCents })
}

/**
 * 积分明细 (GET /v1/wxshop/member/points?limit=; CONTRACT §16.3(10))。
 * points: 当前积分(权威运行余额); logs: 流水(倒序, limit 默认 50, ≤200)。
 * 降级: 真接口失败 → 回 {points:0, logs:[]} (页面空态, 不阻断)。
 */
export async function getPoints(limit?: number): Promise<PointsResult> {
  if (USE_MOCK) {
    return {
      points: 320,
      logs: [
        { type: 'earn', delta: 50, points_after: 320, ref_order_id: 1001, created_at: '2026-07-28T10:20:00+08:00' },
        { type: 'redeem', delta: -100, points_after: 270, created_at: '2026-07-20T15:00:00+08:00' },
        { type: 'earn', delta: 150, points_after: 370, ref_order_id: 980, created_at: '2026-07-10T09:30:00+08:00' },
      ],
    }
  }
  try {
    const qs = limit && limit > 0 ? `?limit=${limit}` : ''
    return await http.get<PointsResult>(`/v1/wxshop/member/points${qs}`)
  } catch (err) {
    // 降级: 真接口失败 → 回空积分 (页面空态, 不阻断)
    console.warn('[member] getPoints 真接口失败,降级 0/[]', err)
    return { points: 0, logs: [] }
  }
}
