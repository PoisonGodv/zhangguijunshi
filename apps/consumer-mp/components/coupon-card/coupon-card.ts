// components/coupon-card/coupon-card.ts — 券卡片组件
import type { Coupon } from '../../types/api'

const STATUS_TEXT: Record<string, string> = {
  unused: '未使用',
  used: '已使用',
  expired: '已过期',
}

Component({
  properties: {
    coupon: { type: Object, value: {} as Coupon },
  },
  data: {
    statusText: '',
  },
  observers: {
    'coupon.status'(status: string) {
      this.setData({ statusText: STATUS_TEXT[status] || status })
    },
  },
  lifetimes: {
    attached() {
      const c = this.data.coupon as Coupon
      this.setData({ statusText: STATUS_TEXT[c.status] || c.status || '' })
    },
  },
  methods: {
    onTap() {
      this.triggerEvent('tap', { coupon: this.data.coupon })
    },
  },
})
