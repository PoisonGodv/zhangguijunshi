// pages/coupons/coupons.ts — 券包(tabBar 页)
import * as coupon from '../../services/coupon'
import { ApiError } from '../../utils/request'
import type { Coupon } from '../../types/api'

type CouponStatus = 'unused' | 'used' | 'expired'

Page({
  data: {
    currentTab: 'unused' as CouponStatus,
    coupons: [] as Coupon[],
    loading: true,
    tabs: [
      { status: 'unused', label: '未使用' },
      { status: 'used', label: '已使用' },
      { status: 'expired', label: '已过期' },
    ],
  },

  onLoad() {
    this.load(this.data.currentTab)
  },

  onPullDownRefresh() {
    this.load(this.data.currentTab).finally(() => wx.stopPullDownRefresh())
  },

  async load(status: CouponStatus) {
    this.setData({ loading: true })
    try {
      const list = await coupon.listCoupons(status)
      this.setData({ coupons: list, loading: false })
    } catch (e) {
      this.setData({ coupons: [], loading: false })
      const msg = e instanceof ApiError ? e.message : '加载失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  onTabTap(e: { currentTarget: { dataset: { status: CouponStatus } } }) {
    const status = e.currentTarget.dataset.status
    if (status === this.data.currentTab) return
    this.setData({ currentTab: status })
    this.load(status)
  },

  onTapCoupon() {
    wx.showToast({ title: '券详情即将上线', icon: 'none' })
  },
})
