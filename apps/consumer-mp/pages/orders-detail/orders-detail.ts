// pages/orders-detail/orders-detail.ts — 订单详情 (CONTRACT §24.3)
//
// 数据流:
//   onLoad(query.id) → getOrderDetail(id) → 渲染详情
//   失败: service 降级 null → toast + navigateBack
// 来源: orders 页 onTapOrder → navigateTo orders-detail?id=
import { getOrderDetail } from '../../services/order'
import { yuan, fmtDateTime } from '../../utils/format'
import { ApiError } from '../../utils/request'
import type { OrderDetail } from '../../types/api'

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

interface DisplayDetail {
  id: number
  order_no: string
  status_key: OrderStatus
  status_text: string
  status_cls: string
  total: string
  paid: string
  hasPaid: boolean
  channel: string
  payment_method: string
  created: string
  paidAt: string
  items: DisplayItem[]
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
      wx.showToast({ title: '订单不存在', icon: 'none' })
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
      const d = await getOrderDetail(this._id)
      if (!d) {
        // service 降级 null: toast 退回
        this.setData({ loading: false, detail: null })
        wx.showToast({ title: '订单不存在', icon: 'none' })
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

  _adapt(d: OrderDetail): DisplayDetail {
    const items: DisplayItem[] = (d.items || []).map((it) => {
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
    const status = d.status as OrderStatus
    return {
      id: d.id,
      order_no: d.order_no,
      status_key: status,
      status_text: STATUS_TEXT[status] || d.status,
      status_cls: `order-status order-status-${d.status}`,
      total: yuan(d.total_cents),
      paid: yuan(d.paid_cents),
      hasPaid: d.paid_cents > 0,
      channel: d.channel || '—',
      payment_method: d.payment_method || '—',
      created: fmtDateTime(d.created_at),
      paidAt: d.paid_at ? fmtDateTime(d.paid_at) : '—',
      items,
    }
  },
})
