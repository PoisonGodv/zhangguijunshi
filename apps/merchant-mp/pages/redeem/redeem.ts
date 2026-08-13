// pages/redeem/redeem.ts — 会员码核销页 (MVP-4)
//
// 数据流:
//   「扫码」→ wx.scanCode 扫顾客出示的动态会员码 → 填入 code (也可手输)
//   填消费金额 (元→分 consume_cents, 扣储值) + 可选积分 (earn_points, 累积分)
//   「确认核销」→ showModal 二次确认 → redeemMemberCode({code, consume_cents?, earn_points?})
//     (POST /v1/boss/membership/redeem) → 回执 (本次扣储值 + 核销后余额/积分)。
//   失败 toast errcode 中文 (40003 码无效/过期 / 60004 余额不足 / 42302 冻结 / 40001 参数错)。
//
// 后端字段说明 (MVP-4 已读源码确认):
//   handler.redeemRequest 有 json tag → snake_case: {code(必填), consume_cents?, earn_points?}。
//   membership.RedeemResult 无 json tag → PascalCase (MemberID/Points/BalanceCents/ConsumedCents)。
//   code/consume_cents/earn_points 校验在后端 service.Redeem: code 必填, consume/earn 至少一个 >0。
//
// 重要: 核销只记储值流水 (consume) + 积分流水 (earn),**不产生 billing.Order 销售单**。
//   因此 C 端「我的积分/余额」会更新,但「我的订单」不会出现核销单 —— 本页文案已注明。
// ARCH-01: tenant/store/operator 永远由后端从 JWT ctx 解析,前端只传 code + 金额/积分。

import {
  redeemMemberCode,
  getAccessToken,
  ApiError,
} from '../../utils/request'
import type { RedeemResult } from '../../types/api'

/** 回执视图模型 */
interface ReceiptVM {
  member_id: number
  consumed_yuan: string // 本次扣储值
  balance_yuan: string // 核销后实付余额
  points: number // 核销后积分
}

interface RedeemData {
  code: string
  scanning: boolean
  consumeYuan: string // 消费金额输入 (元)
  consumeCents: number // 解析后 (分)
  earnPoints: string // 积分输入 (原始字符串)
  submitting: boolean
  receipt: ReceiptVM | null
}

Page({
  data: {
    code: '',
    scanning: false,
    consumeYuan: '',
    consumeCents: 0,
    earnPoints: '',
    submitting: false,
    receipt: null,
  } as RedeemData,

  onLoad() {
    // 登录守卫 (FE-MP-05): 无 token 或 refresh 也失败 → 去登录页
    if (!getAccessToken()) {
      wx.redirectTo({ url: '/pages/login/login' })
      return
    }
  },

  // === 扫码 → 动态会员码 ===
  onScan() {
    const d = this.data as RedeemData
    if (d.scanning) return
    this.setData({ scanning: true })
    wx.scanCode({
      success: (r) => {
        const code = ((r && r.result) || '').trim()
        if (!code) {
          wx.showToast({ title: '未识别会员码', icon: 'none' })
          return
        }
        this.setData({ code })
      },
      fail: () => {
        // 用户取消扫码不打扰
      },
      complete: () => {
        this.setData({ scanning: false })
      },
    })
  },

  // === 手输会员码 ===
  onCodeInput(e: { detail: { value: string } }) {
    this.setData({ code: (e.detail && e.detail.value) || '' })
  },

  // === 消费金额 (元 → 分) ===
  onConsumeInput(e: { detail: { value: string } }) {
    const raw = (e.detail && e.detail.value) || ''
    this.setData({ consumeYuan: raw, consumeCents: parseYuanToCents(raw) })
  },

  // === 积分 (可选, 正整数) ===
  onEarnInput(e: { detail: { value: string } }) {
    const raw = (e.detail && e.detail.value) || ''
    this.setData({ earnPoints: raw.replace(/[^\d]/g, '') })
  },

  // === 确认核销 → 二次确认 → 调接口 → 回执 ===
  onConfirm(): void {
    const d = this.data as RedeemData
    const code = (d.code || '').trim()
    const consumeCents = d.consumeCents
    const earnPoints = parseInt(d.earnPoints || '0', 10) || 0
    if (!code || d.submitting) return
    if (consumeCents <= 0 && earnPoints <= 0) {
      wx.showToast({ title: '请填消费金额或积分', icon: 'none' })
      return
    }
    const parts: string[] = []
    if (consumeCents > 0) parts.push(`扣储值 ¥${yuan(consumeCents)}`)
    if (earnPoints > 0) parts.push(`积 ${earnPoints} 分`)
    wx.showModal({
      title: '确认核销',
      content: `${parts.join(' + ')}\n(核销计储值与积分,不开销售单)`,
      confirmText: '确认核销',
      confirmColor: '#FF6B35',
      success: (r) => {
        if (!r.confirm) return
        this.doRedeem(code, consumeCents, earnPoints)
      },
    })
  },

  // === 内部: 执行核销 ===
  async doRedeem(code: string, consumeCents: number, earnPoints: number): Promise<void> {
    this.setData({ submitting: true })
    try {
      const res = await redeemMemberCode({
        code,
        consume_cents: consumeCents > 0 ? consumeCents : undefined,
        earn_points: earnPoints > 0 ? earnPoints : undefined,
      })
      this.setData({ receipt: buildReceipt(res) })
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '核销失败'
      wx.showToast({ title: msg, icon: 'none' })
    } finally {
      this.setData({ submitting: false })
    }
  },

  // === 回执「再核一笔」 → 回输入态 ===
  onNewRedeem() {
    this.setData({
      receipt: null,
      code: '',
      consumeYuan: '',
      consumeCents: 0,
      earnPoints: '',
    })
  },
})

/** 后端金额 int64 分 → 元(整除显示整数,否则两位小数),与 me/billing/today 对齐 */
function yuan(cents: number | string): string {
  const v = Number(cents) / 100
  return v % 1 === 0 ? String(v) : v.toFixed(2)
}

/** 自定义输入 元字符串 → 分 (两位小数;非法/空/负 → 0),与 recharge 对齐 */
function parseYuanToCents(raw: string): number {
  const s = (raw || '').trim()
  if (!s) return 0
  const n = Number(s)
  if (!isFinite(n) || n <= 0) return 0
  return Math.round(n * 100)
}

/** 构造回执视图模型 (本次扣储值 + 核销后余额/积分) */
function buildReceipt(r: RedeemResult): ReceiptVM {
  return {
    member_id: r.MemberID,
    consumed_yuan: yuan(r.ConsumedCents || 0),
    balance_yuan: yuan(r.BalanceCents || 0),
    points: Number(r.Points || 0),
  }
}
