// pages/task-detail/task-detail.ts — 单条建议详情 + 一键执行 (FE-MP-03 核心)
//
// 数据流: onLoad(id,itemId) → getStrategy(id) (GET /v1/boss/aiengine/strategies/:id)
//         → 从 strategy.items 找 itemId → 渲染详情
// tap「一键执行」→ executeItem(id, itemId, {confirm:true})
//         (POST /v1/boss/aiengine/strategies/:id/items/:itemId/execute)
//         → 按 ExecResult.status 三分支:
//            executed → showToast「已执行」+ navigateBack
//            blocked  → showModal「未执行:<reason中文>」
//            failed   → showToast 错误
//         action_type==recharge_promo → 按钮置灰「暂不支持」

import { getStrategy, executeItem, ApiError } from '../../utils/request'
import type { StrategyItem, DetailVM } from '../../types/api'

// === action_type → 中文 (CONTRACT §12.3.0) ===
const ACTION_TYPE_LABEL: Record<string, string> = {
  group_seckill: '群秒杀',
  coupon_wake: '券唤醒',
  moments_post: '朋友圈',
  price_adj: '调价',
  groupbuy: '拼团',
  recharge_promo: '储值促销',
}

// === 执行拦截 reason → 中文 (CONTRACT §12.3.9 ExecResult.reason + §11.3.2 blocked_reason) ===
const REASON_LABEL: Record<string, string> = {
  margin_floor: '毛利护栏拦截',
  freq_circuit: '频次熔断',
  content_filter: '内容含敏感词',
  unsubscribed: '会员已退订',
  subscribe_quota: '订阅次数用尽',
  confirm_required: '需二次确认',
  not_executable: '暂不支持',
  not_configured: '通道未配置',
}

interface TaskDetailData {
  loading: boolean
  detail: DetailVM | null
  executing: boolean
  btnDisabled: boolean
  btnText: string
}

Page({
  data: {
    loading: true,
    detail: null,
    executing: false,
    btnDisabled: false,
    btnText: '一键执行',
  } as TaskDetailData,

  onLoad(query: Record<string, string | undefined>) {
    const id = Number(query.id)
    const itemId = Number(query.itemId)
    if (!id || !itemId) {
      wx.showToast({ title: '参数错误', icon: 'none' })
      this.setData({ loading: false })
      return
    }
    this.load(id, itemId)
  },

  async load(id: number, itemId: number): Promise<void> {
    try {
      const strategy = await getStrategy(id)
      const item: StrategyItem | undefined = (strategy.items || []).find((it) => it.id === itemId)
      if (!item) {
        wx.showToast({ title: '建议不存在', icon: 'none' })
        this.setData({ loading: false })
        return
      }
      const executed = item.execution_status === 'executed'
      const isRecharge = item.action_type === 'recharge_promo'
      const vm: DetailVM = {
        itemId: item.id,
        strategyId: id,
        actionLabel: ACTION_TYPE_LABEL[item.action_type] || item.action_type,
        reason: item.reason || '',
        copy: item.copy || '',
        paramsText: formatParams(item.action_type, item.params),
        targetText: formatTarget(item.target),
        effectText: formatEffect(item.expected_effect),
        marginOk: !!item.margin_ok,
        actionType: item.action_type,
        executed,
      }
      this.setData({
        loading: false,
        detail: vm,
        btnDisabled: executed || isRecharge,
        btnText: executed ? '已执行' : isRecharge ? '暂不支持' : '一键执行',
      })
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '加载失败'
      wx.showToast({ title: msg, icon: 'none' })
      this.setData({ loading: false })
    }
  },

  async onExecute(): Promise<void> {
    const detail: DetailVM | null = (this.data as TaskDetailData).detail
    if (!detail || (this.data as TaskDetailData).btnDisabled || (this.data as TaskDetailData).executing) {
      return
    }
    this.setData({ executing: true })
    try {
      // 统一传 confirm:true(price_adj 必需,其他 action_type 后端忽略)
      const res = await executeItem(detail.strategyId, detail.itemId, { confirm: true })
      if (res.status === 'executed') {
        wx.showToast({ title: '已执行', icon: 'success' })
        this.setData({ btnDisabled: true, btnText: '已执行' })
        setTimeout(() => wx.navigateBack(), 800)
      } else if (res.status === 'blocked') {
        const reasonZh = REASON_LABEL[res.reason || ''] || res.reason || '未执行'
        wx.showModal({
          title: '未执行',
          content: reasonZh,
          showCancel: false,
          confirmText: '知道了',
        })
      } else {
        // failed
        const reasonZh = REASON_LABEL[res.reason || ''] || res.reason || '执行失败'
        wx.showToast({ title: reasonZh, icon: 'none' })
      }
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '执行失败'
      wx.showToast({ title: msg, icon: 'none' })
    } finally {
      this.setData({ executing: false })
    }
  },
})

// === params 人类可读化 (CONTRACT §10.3 CouponParams + §12.3.1 StrategyItem.params) ===
function formatParams(actionType: string, p: Record<string, any> | undefined): string {
  if (!p || typeof p !== 'object') return '-'
  // 满减
  if (p.threshold_cents != null && p.off_cents != null) {
    return `满${yuan(p.threshold_cents)}减${yuan(p.off_cents)}`
  }
  // 折扣 (permille ‰,850=8.5折)
  if (p.discount_permille != null) {
    const zhe = (Number(p.discount_permille) / 100).toFixed(1).replace(/\.0$/, '')
    return `${zhe}折`
  }
  // 代金直减
  if (p.off_cents != null) {
    return `立减¥${yuan(p.off_cents)}`
  }
  // 改价
  if (actionType === 'price_adj' && p.new_price_cents != null) {
    return `改价为 ¥${yuan(p.new_price_cents)}`
  }
  // 拼团
  if (actionType === 'groupbuy') {
    const parts: string[] = []
    if (p.group_size) parts.push(`${p.group_size}人团`)
    if (p.price_cents != null) parts.push(`团价¥${yuan(p.price_cents)}`)
    if (p.valid_hours) parts.push(`${p.valid_hours}小时有效`)
    if (parts.length) return parts.join(' · ')
  }
  if (p.limit_qty_max != null) return `限购${p.limit_qty_max}份`
  if (p.expire_hours != null) return `${p.expire_hours}小时内有效`
  // 兜底:原样展示 k=v
  const keys = Object.keys(p)
  if (!keys.length) return '-'
  return keys.map((k) => `${k}=${p[k]}`).join(' · ')
}

// === target 受众人类可读化 ===
function formatTarget(t: Record<string, any> | undefined): string {
  if (!t || typeof t !== 'object') return '全部会员'
  if (t.segment) return String(t.segment)
  if (t.member_count != null) return `${t.member_count}位会员`
  if (t.count != null) return `${t.count}人`
  if (t.product) return `商品:${t.product}`
  if (t.product_id) return `商品ID:${t.product_id}`
  const keys = Object.keys(t)
  if (!keys.length) return '全部会员'
  return keys.map((k) => `${k}=${t[k]}`).join(' · ')
}

// === expected_effect 人类可读化 (兼容 string/object) ===
function formatEffect(eff: Record<string, any> | string | undefined): string {
  if (!eff) return '-'
  if (typeof eff === 'string') return eff || '-'
  const parts: string[] = []
  if (eff.summary) parts.push(String(eff.summary))
  if (eff.desc) parts.push(String(eff.desc))
  if (eff.redeem_count != null) parts.push(`预计核销${eff.redeem_count}份`)
  if (eff.recover_cents != null) parts.push(`挽回¥${yuan(eff.recover_cents)}`)
  if (eff.reach_count != null) parts.push(`触达${eff.reach_count}人`)
  if (eff.est_revenue_cents != null) parts.push(`预计增收¥${yuan(eff.est_revenue_cents)}`)
  return parts.length ? parts.join(' · ') : '-'
}

/** 后端金额 int64 分 → 元(整除显示整数,否则两位小数) */
function yuan(cents: number | string): string {
  const v = Number(cents) / 100
  return v % 1 === 0 ? String(v) : v.toFixed(2)
}
