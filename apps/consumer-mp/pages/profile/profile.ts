// pages/profile/profile.ts — 我的(tabBar 页)
import { userStore, setUserMember } from '../../stores/user'
import * as member from '../../services/member'
import { yuan } from '../../utils/format'

Page({
  data: {
    isLoggedIn: false,
    levelName: '游客',
    points: 0,
    balanceYuan: '0',
    totalSpentYuan: '0',
    phone: '',
    entries: [
      { key: 'member-code', icon: '🎫', label: '会员码' },
      { key: 'coupons', icon: '🧧', label: '券包' },
      { key: 'orders', icon: '📋', label: '订单' },
      { key: 'balance', icon: '💰', label: '储值' },
      { key: 'points', icon: '⭐', label: '积分' },
      { key: 'stores', icon: '🏪', label: '门店' },
      { key: 'service', icon: '💬', label: '客服' },
      { key: 'settings', icon: '⚙️', label: '设置' },
    ],
  },

  _unsub: null as null | (() => void),

  onLoad() {
    this.setData(this._snap(userStore.get()))
    this._unsub = userStore.subscribe((s) => this.setData(this._snap(s)))
  },

  onUnload() {
    if (this._unsub) this._unsub()
  },

  onShow() {
    if (userStore.get().accessToken) {
      member
        .getMyProfile()
        .then((info) => setUserMember(info))
        .catch(() => {})
    }
  },

  _snap(s: ReturnType<typeof userStore.get>) {
    return {
      isLoggedIn: !!s.accessToken,
      levelName: s.member?.level_name || '游客',
      points: s.member?.points || 0,
      balanceYuan: s.member ? yuan(s.member.balance_cents) : '0',
      totalSpentYuan: s.member ? yuan(s.member.total_spent_cents) : '0',
      phone: s.member?.phone_masked || '',
    }
  },

  onEntry(e: { currentTarget: { dataset: { key: string } } }) {
    const key = e.currentTarget.dataset.key
    if (key === 'member-code') {
      wx.navigateTo({ url: '/pages/member-code/member-code' })
    } else if (key === 'coupons') {
      wx.switchTab({ url: '/pages/coupons/coupons' })
    } else if (key === 'orders') {
      wx.navigateTo({ url: '/pages/orders/orders' })
    } else if (key === 'balance') {
      wx.navigateTo({ url: '/pages/balance/balance' })
    } else if (key === 'points') {
      wx.navigateTo({ url: '/pages/points/points' })
    } else if (key === 'stores') {
      wx.navigateTo({ url: '/pages/stores/stores' })
    } else {
      // service/settings: C2/后续补
      wx.showToast({ title: '即将上线', icon: 'none' })
    }
  },

  onTapLogin() {
    wx.navigateTo({ url: '/pages/join/join' })
  },
})
