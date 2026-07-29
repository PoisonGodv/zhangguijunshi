// pages/points/points.ts — 积分明细 (C2)
//
// 数据流:
//   onLoad / onPullDownRefresh → getPoints() → 顶部当前积分 + 下方明细按日期分组
//   失败: service 已降级 {points:0, logs:[]}, 页面空态
// 分组: logs 倒序, 按日期(YYYY-MM-DD) 归组, 每条 type/delta(+绿/-红)/points_after/created_at
import * as member from '../../services/member'
import { userStore } from '../../stores/user'
import { fmtDate, fmtDateTime } from '../../utils/format'
import { ApiError } from '../../utils/request'
import type { PointLog, PointsResult } from '../../types/api'

const TYPE_TEXT: Record<string, string> = {
  earn: '消费获得',
  redeem: '兑换使用',
  expire: '积分过期',
  adjust: '调整',
}

interface DisplayLog {
  key: string
  typeText: string
  deltaText: string   // +50 / -100
  deltaCls: string    // points-delta-pos / points-delta-neg
  pointsAfter: number
  created: string     // YYYY-MM-DD HH:mm:ss
}

interface DisplayGroup {
  date: string   // YYYY-MM-DD
  logs: DisplayLog[]
}

Page({
  data: {
    isLoggedIn: false,
    loading: true,
    points: 0,
    groups: [] as DisplayGroup[],
  },

  _unsub: null as null | (() => void),

  onLoad() {
    this.setData({ isLoggedIn: !!userStore.get().accessToken })
    this._unsub = userStore.subscribe((s) => this.setData({ isLoggedIn: !!s.accessToken }))
    if (!userStore.get().accessToken) {
      this.setData({ loading: false })
      return
    }
    this.load()
  },

  onUnload() {
    if (this._unsub) this._unsub()
  },

  onPullDownRefresh() {
    this.load().finally(() => wx.stopPullDownRefresh())
  },

  async load(): Promise<void> {
    this.setData({ loading: true })
    try {
      const r: PointsResult = await member.getPoints()
      this.setData({
        loading: false,
        points: r.points,
        groups: this._group(r.logs || []),
      })
    } catch (e) {
      // getPoints 内部已降级 0/[]; 防御性兜底
      this.setData({ loading: false, points: 0, groups: [] })
      const msg = e instanceof ApiError ? e.message : '加载失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  _group(logs: PointLog[]): DisplayGroup[] {
    const map = new Map<string, DisplayLog[]>()
    const order: string[] = []
    for (const l of logs) {
      const date = fmtDate(l.created_at) || '未知日期'
      if (!map.has(date)) {
        map.set(date, [])
        order.push(date)
      }
      const delta = l.delta || 0
      map.get(date)!.push({
        key: `${l.created_at}-${l.type}-${l.delta}`,
        typeText: TYPE_TEXT[l.type] || l.type,
        deltaText: (delta > 0 ? '+' : '') + delta,
        deltaCls: delta >= 0 ? 'points-delta-pos' : 'points-delta-neg',
        pointsAfter: l.points_after,
        created: fmtDateTime(l.created_at),
      })
    }
    return order.map((date) => ({ date, logs: map.get(date)! }))
  },

  onTapLogin() {
    wx.navigateTo({ url: '/pages/join/join' })
  },
})
