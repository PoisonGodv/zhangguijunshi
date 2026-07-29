// pages/group-buy-detail/group-buy-detail.ts — 拼团详情+参团 (C2)
//
// 数据流:
//   onLoad(query.id) → getGroupBuyDetail(id) → 渲染详情
//   失败: service 降级 null → toast + navigateBack
// 事件:
//   onJoin → joinGroupBuy(id) 写操作; 成功 toast 刷新; 60013 满员/60015 重复/60014 结束 已映射中文
//   按钮在 满员(joined>=required) / 已结束(status!=='active') 时禁用
import { getGroupBuyDetail, joinGroupBuy } from '../../services/group-buy'
import { userStore } from '../../stores/user'
import { yuan, fmtDateTime } from '../../utils/format'
import { ApiError } from '../../utils/request'
import type { GroupBuyDetail } from '../../types/api'

const STATUS_TEXT: Record<string, string> = {
  active: '进行中',
  success: '已成团',
  failed: '未成团',
  cancelled: '已取消',
}

interface DisplayDetail {
  id: number
  title: string
  desc: string
  priceYuan: string
  originalYuan: string
  required: number
  joined: number
  progress: number
  status: string
  statusText: string
  expire: string
  canJoin: boolean
  joinDisabledReason: string
}

Page({
  data: {
    loading: true,
    detail: null as DisplayDetail | null,
    joining: false,
  },

  _id: 0,

  onLoad(q: { id?: string }) {
    const id = parseInt(String(q.id || ''), 10)
    if (!id) {
      wx.showToast({ title: '拼团不存在', icon: 'none' })
      setTimeout(() => wx.navigateBack(), 800)
      return
    }
    this._id = id
    this.load()
  },

  onPullDownRefresh() {
    this.load().finally(() => wx.stopPullDownRefresh())
  },

  async load(): Promise<void> {
    if (!this._id) return
    this.setData({ loading: true })
    try {
      const d = await getGroupBuyDetail(this._id)
      if (!d) {
        // service 降级 null: toast 退回
        this.setData({ loading: false, detail: null })
        wx.showToast({ title: '拼团不存在', icon: 'none' })
        setTimeout(() => wx.navigateBack(), 800)
        return
      }
      this.setData({ loading: false, detail: this._adapt(d) })
    } catch (e) {
      this.setData({ loading: false, detail: null })
      const msg = e instanceof ApiError ? e.message : '加载失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  _adapt(d: GroupBuyDetail): DisplayDetail {
    const progress = d.required > 0 ? Math.min(100, Math.round((d.joined / d.required) * 100)) : 0
    const full = d.required > 0 && d.joined >= d.required
    const active = d.status === 'active'
    let joinDisabledReason = ''
    if (!active) joinDisabledReason = '拼团已结束'
    else if (full) joinDisabledReason = '已满员'
    return {
      id: d.id,
      title: d.title,
      desc: d.desc || d.title,
      priceYuan: yuan(d.price_cents),
      originalYuan: yuan(d.original_cents),
      required: d.required,
      joined: d.joined,
      progress,
      status: d.status,
      statusText: STATUS_TEXT[d.status] || d.status,
      expire: fmtDateTime(d.expire),
      canJoin: active && !full,
      joinDisabledReason,
    }
  },

  async onJoin() {
    if (!this._id) return
    if (!this.data.detail || !this.data.detail.canJoin || this.data.joining) return
    // 未登录 → 引导入会 (参团需 JWT)
    if (!userStore.get().accessToken) {
      wx.showToast({ title: '请先领券入会再参团', icon: 'none' })
      wx.navigateTo({ url: '/pages/join/join' })
      return
    }
    this.setData({ joining: true })
    try {
      wx.showLoading({ title: '参团中...', mask: true })
      await joinGroupBuy(this._id)
      wx.hideLoading()
      wx.showToast({ title: '参团成功', icon: 'success' })
      this.setData({ joining: false })
      this.load() // 刷新进度
    } catch (err) {
      wx.hideLoading()
      this.setData({ joining: false })
      // 60013 满员/60015 重复/60014 结束 已映射中文; 其余用后端 message
      const msg = err instanceof ApiError ? err.message : '参团失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },
})
