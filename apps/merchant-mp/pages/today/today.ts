// pages/today/today.ts — 「今日3件事」首页 (FE-MP-03 核心)
//
// 数据流: onLoad / 下拉刷新 → getToday() (POST /v1/boss/aiengine/today) → envelope.data:Advice
//         → 渲染 advice.strategy.items (按 rank 排序,取前 3) 为卡片
//         → degrade 非空且非 cache_hit → 顶部黄色提示条
//         → items 空 → 空状态
//         → tap 卡片 → navigateTo task-detail?id=strategy.id&itemId=item.id

import { getToday, ApiError } from '../../utils/request'
import type { Advice, StrategyItem, AdviceCardVM } from '../../types/api'
const ACTION_TYPE_LABEL: Record<string, string> = {
  group_seckill: '群秒杀',
  coupon_wake: '券唤醒',
  moments_post: '朋友圈',
  price_adj: '调价',
  groupbuy: '拼团',
  recharge_promo: '储值促销',
}

// === action_type → 卡片标签色块 class (app.wxss 定义) ===
const ACTION_TAG_CLASS: Record<string, string> = {
  group_seckill: 'tag-seckill',
  coupon_wake: 'tag-wake',
  moments_post: 'tag-moments',
  price_adj: 'tag-price',
  groupbuy: 'tag-group',
  recharge_promo: 'tag-recharge',
}

// === degrade → 提示文案 (CONTRACT §12.3.7;cache_hit 不提示) ===
const DEGRADE_HINT: Record<string, string> = {
  budget: 'AI 预算已达上限,使用规则建议',
  llm: 'AI 增强暂时异常,使用规则建议',
  no_provider: 'AI 未配置,使用规则建议',
  cold_start: '数据不足,使用冷启动剧本',
  cache_hit: '', // 缓存命中,不提示
}

interface TodayData {
  loading: boolean
  degradeHint: string
  cards: AdviceCardVM[]
}

Page({
  data: {
    loading: true,
    degradeHint: '',
    cards: [] as AdviceCardVM[],
  } as TodayData,

  onLoad() {
    this.load()
  },

  onPullDownRefresh() {
    this.load().finally(() => wx.stopPullDownRefresh())
  },

  async load(): Promise<void> {
    this.setData({ loading: true })
    try {
      const advice: Advice = await getToday()
      const items: StrategyItem[] = (advice.strategy && advice.strategy.items) || []
      const cards: AdviceCardVM[] = items
        .slice()
        .sort((a, b) => a.rank - b.rank)
        .slice(0, 3)
        .map((it) => ({
          id: it.id,
          strategyId: (advice.strategy && advice.strategy.id) || 0,
          rank: it.rank,
          actionLabel: ACTION_TYPE_LABEL[it.action_type] || it.action_type,
          actionTagClass: ACTION_TAG_CLASS[it.action_type] || 'tag-wake',
          copy: it.copy || '',
          reason: it.reason || '',
          effect: summarizeEffect(it.expected_effect),
          marginOk: !!it.margin_ok,
        }))

      const dg = advice.degrade || ''
      this.setData({
        loading: false,
        cards,
        degradeHint: DEGRADE_HINT[dg] || '',
      })
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '加载失败,下拉重试'
      this.setData({ loading: false, cards: [], degradeHint: '' })
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  onTapCard(e: { currentTarget: { dataset: Record<string, unknown> } }) {
    const id = Number(e.currentTarget.dataset.id)
    const sid = Number(e.currentTarget.dataset.sid)
    if (!id || !sid) return
    wx.navigateTo({ url: `/pages/task-detail/task-detail?id=${sid}&itemId=${id}` })
  },
})

// === expected_effect 摘要 (兼容 string / object,见 types/api.d.ts 注释) ===
function summarizeEffect(eff: Record<string, any> | string | undefined): string {
  if (!eff) return ''
  if (typeof eff === 'string') return eff
  const parts: string[] = []
  if (eff.summary) parts.push(String(eff.summary))
  if (eff.desc) parts.push(String(eff.desc))
  if (eff.redeem_count != null) parts.push(`预计核销${eff.redeem_count}份`)
  if (eff.recover_cents != null) parts.push(`挽回¥${yuan(eff.recover_cents)}`)
  if (eff.reach_count != null) parts.push(`触达${eff.reach_count}人`)
  if (eff.est_revenue_cents != null) parts.push(`预计增收¥${yuan(eff.est_revenue_cents)}`)
  return parts.join(' · ')
}

/** 后端金额 int64 分 → 元(整除显示整数,否则两位小数) */
function yuan(cents: number | string): string {
  const v = Number(cents) / 100
  return v % 1 === 0 ? String(v) : v.toFixed(2)
}
