// pages/coupon-detail/coupon-detail.ts — 券详情 (C2)
//
// 数据流:
//   onLoad(query.id) → getCouponDetail(id) → 渲染详情
//   失败: service 降级 null → toast + navigateBack
// 事件:
//   onClaim → 本券详情不直接领 (claim-newcomer 仅对新客; 本券 available 提示到店核销领取)
// 点击 coupons 页券卡片 → navigateTo coupon-detail?id= (修改 coupons.ts onTapCoupon)
import { getCouponDetail } from '../../services/coupon'
import { fmtDate } from '../../utils/format'
import { ApiError } from '../../utils/request'
import type { CouponDetail } from '../../types/api'

const TYPE_TEXT: Record<string, string> = {
  cash: '代金券',
  discount: '折扣券',
  gift: '礼品券',
}

const STATUS_TEXT: Record<string, string> = {
  unused: '未使用',
  used: '已使用',
  expired: '已过期',
  available: '可领取',
}

interface DisplayDetail {
  id: number
  title: string
  type: string
  typeText: string
  valueText: string
  status: string
  statusText: string
  thresholdText: string
  scopeText: string
  valueTextDetail: string
  validFrom: string
  validTo: string
  available: boolean
}

Page({
  data: {
    loading: true,
    detail: null as DisplayDetail | null,
  },

  _id: 0,

  onLoad(q: { id?: string }) {
    const id = parseInt(String(q.id || ''), 10)
    if (!id) {
      wx.showToast({ title: '券不存在', icon: 'none' })
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
      const d = await getCouponDetail(this._id)
      if (!d) {
        this.setData({ loading: false, detail: null })
        wx.showToast({ title: '券不存在', icon: 'none' })
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

  _adapt(d: CouponDetail): DisplayDetail {
    return {
      id: d.id,
      title: d.title,
      type: d.type,
      typeText: TYPE_TEXT[d.type] || d.type,
      valueText: d.value_text,
      status: d.status,
      statusText: STATUS_TEXT[d.status] || d.status,
      thresholdText: d.detail.threshold_text || (d.threshold_text || '无门槛'),
      scopeText: d.detail.scope_text || (d.scope_text || '全场通用'),
      valueTextDetail: d.detail.value_text || d.value_text,
      validFrom: d.detail.valid_from ? fmtDate(d.detail.valid_from) : '',
      validTo: d.detail.valid_to ? fmtDate(d.detail.valid_to) : '',
      available: !!d.available,
    }
  },

  onClaim() {
    // 本券详情不直接领取: claim-newcomer 仅对新客; 本券 available 提示到店核销领取。
    wx.showToast({ title: '到店核销领取', icon: 'none' })
  },
})
