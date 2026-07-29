// pages/group-buy/group-buy.ts — 拼团列表页 (C2)
//
// 数据流:
//   onLoad / onPullDownRefresh → listGroupBuys() 拉进行中拼团 → 卡片渲染
//   失败: 降级空态 + toast (service 内已降级 mock, 页面兜底 toast)
// 事件:
//   onTapItem → navigateTo /pages/group-buy-detail/group-buy-detail?id=
import { listGroupBuys } from '../../services/group-buy'
import { yuan } from '../../utils/format'
import { ApiError } from '../../utils/request'
import type { GroupBuyBrief } from '../../types/api'

interface DisplayGroupBuy extends GroupBuyBrief {
  priceYuan: string
  originalYuan: string
  progress: number   // joined / required 占比 0~100
}

Page({
  data: {
    loading: true,
    list: [] as DisplayGroupBuy[],
  },

  onLoad() {
    this.load()
  },

  onPullDownRefresh() {
    this.load().finally(() => wx.stopPullDownRefresh())
  },

  async load(): Promise<void> {
    this.setData({ loading: true })
    try {
      const list = await listGroupBuys()
      this.setData({
        loading: false,
        list: list.map((g) => this._adapt(g)),
      })
    } catch (e) {
      this.setData({ loading: false, list: [] })
      const msg = e instanceof ApiError ? e.message : '加载失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  _adapt(g: GroupBuyBrief): DisplayGroupBuy {
    const progress = g.required > 0 ? Math.min(100, Math.round((g.joined / g.required) * 100)) : 0
    return {
      ...g,
      priceYuan: yuan(g.price_cents),
      originalYuan: yuan(g.original_cents),
      progress,
    }
  },

  onTapItem(e: { currentTarget: { dataset: { id: number } } }) {
    const id = e.currentTarget.dataset.id
    if (!id) {
      wx.showToast({ title: '拼团不存在', icon: 'none' })
      return
    }
    wx.navigateTo({ url: `/pages/group-buy-detail/group-buy-detail?id=${id}` })
  },
})
