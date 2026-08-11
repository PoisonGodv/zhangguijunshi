// pages/orders/orders.ts — 我的订单页
import * as order from '../../services/order'
import { userStore } from '../../stores/user'
import { yuan, fmtDate } from '../../utils/format'
import { ApiError } from '../../utils/request'
import type { Order } from '../../types/api'

type OrderStatus = 'pending' | 'paid' | 'refunded' | 'voided'

const STATUS_TEXT: Record<OrderStatus, string> = {
  pending: '待支付',
  paid: '已支付',
  refunded: '已退款',
  voided: '已作废',
}

interface DisplayItem {
  key: string
  name: string
  unit: string
  qty: number
  price: string
  line: string
}

interface DisplayOrder {
  id: number
  order_no: string
  status_key: OrderStatus
  status_text: string
  status_cls: string
  total: string
  paid: string
  created: string
  items: DisplayItem[]
}

Page({
  data: {
    isLoggedIn: false,
    orders: [] as DisplayOrder[],
    loading: true,
  },

  _unsub: null as null | (() => void),

  onLoad() {
    this.setData({ isLoggedIn: !!userStore.get().accessToken })
    this._unsub = userStore.subscribe((s) => this.setData({ isLoggedIn: !!s.accessToken }))
    this.load()
  },

  onUnload() {
    if (this._unsub) this._unsub()
  },

  onPullDownRefresh() {
    this.load().finally(() => wx.stopPullDownRefresh())
  },

  async load() {
    this.setData({ loading: true })
    try {
      const list = await order.listOrders()
      this.setData({ orders: list.map((o) => this._adapt(o)), loading: false })
    } catch (e) {
      this.setData({ orders: [], loading: false })
      const msg = e instanceof ApiError ? e.message : '加载失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  _adapt(o: Order): DisplayOrder {
    const items: DisplayItem[] = (o.items || []).map((it) => {
      const price = yuan(it.price_cents)
      return {
        key: `${it.name}-${it.qty}`,
        name: it.name,
        unit: it.unit,
        qty: it.qty,
        price,
        line: `${it.name} ×${it.qty}${it.unit}`,
      }
    })
    return {
      id: o.id,
      order_no: o.order_no,
      status_key: o.status,
      status_text: STATUS_TEXT[o.status] || o.status,
      status_cls: `order-status order-status-${o.status}`,
      total: yuan(o.total_cents),
      paid: yuan(o.paid_cents),
      created: fmtDate(o.created_at),
      items,
    }
  },

  onTapOrder(e: WechatMiniprogram.TouchEvent) {
    // 订单详情接口已就绪 (GET /v1/wxshop/orders/:id, CONTRACT §24.3)
    const id = Number(e.currentTarget.dataset.id || 0)
    if (!id) {
      wx.showToast({ title: '订单不存在', icon: 'none' })
      return
    }
    wx.navigateTo({ url: '/pages/orders-detail/orders-detail?id=' + id })
  },
})
