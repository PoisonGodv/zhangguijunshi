// pages/stores/stores.ts — 门店列表 (C2)
//
// 数据流:
//   onLoad / onPullDownRefresh → listStores() → 卡片渲染 name/address/phone
//   有 group_qr 显示"进群"按钮: 若为 http(s) URL 用 image 组件预览, 否则 toast 文本
//   失败: service 已降级空数组, 页面空态
import { listStores } from '../../services/store'
import { ApiError } from '../../utils/request'
import type { Store } from '../../types/api'

interface DisplayStore {
  id: number
  name: string
  address: string
  phone: string
  hasQR: boolean
  qrIsImage: boolean   // http(s) URL → 用 image 预览
  qrUrl: string        // 原始 group_qr (图片 URL; 非图片为空)
  qrText: string       // 非图片时的文本
}

function looksLikeImageURL(s: string): boolean {
  return /^https?:\/\//i.test(s)
}

Page({
  data: {
    loading: true,
    list: [] as DisplayStore[],
    qrPreview: '',  // 当前预览的二维码图片URL (非空时显示预览浮层)
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
      const list = await listStores()
      this.setData({
        loading: false,
        list: list.map((s) => this._adapt(s)),
      })
    } catch (e) {
      this.setData({ loading: false, list: [] })
      const msg = e instanceof ApiError ? e.message : '加载失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  _adapt(s: Store): DisplayStore {
    const qr = s.group_qr || ''
    const isImage = looksLikeImageURL(qr)
    return {
      id: s.id,
      name: s.name,
      address: s.address,
      phone: s.phone || '',
      hasQR: !!qr,
      qrIsImage: isImage,
      qrUrl: isImage ? qr : '',
      qrText: isImage ? '' : qr,
    }
  },

  onTapGroup(e: { currentTarget: { dataset: { id: number } } }) {
    const id = e.currentTarget.dataset.id
    const item = this.data.list.find((s) => s.id === id)
    if (!item || !item.hasQR) {
      wx.showToast({ title: '暂无进群二维码', icon: 'none' })
      return
    }
    if (item.qrIsImage && item.qrUrl) {
      // 图片 URL: 弹出预览浮层 (image 组件展示)
      this.setData({ qrPreview: item.qrUrl })
      return
    }
    // 非图片(文本/URL): toast 展示文本
    wx.showToast({ title: '进群二维码:' + item.qrText, icon: 'none', duration: 2000 })
  },

  onPreviewImageTap() {
    // 点击预览浮层关闭
    this.setData({ qrPreview: '' })
  },
})
