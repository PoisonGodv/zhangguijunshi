// pages/balance/balance.ts — 储值余额页
import * as member from '../../services/member'
import { userStore } from '../../stores/user'
import { yuan } from '../../utils/format'
import { ApiError } from '../../utils/request'
import type { Balance } from '../../types/api'

Page({
  data: {
    isLoggedIn: false,
    levelName: '游客',
    balanceYuan: '0',
    giftYuan: '0',
    totalYuan: '0',
    loading: true,
  },

  _unsub: null as null | (() => void),

  onLoad() {
    this.setData(this._snap(userStore.get()))
    this._unsub = userStore.subscribe((s) => this.setData(this._snap(s)))
    if (!userStore.get().accessToken) {
      this.setData({ loading: false })
      return
    }
    this.load()
  },

  onUnload() {
    if (this._unsub) this._unsub()
  },

  _snap(s: ReturnType<typeof userStore.get>) {
    return {
      isLoggedIn: !!s.accessToken,
      levelName: s.member?.level_name || '游客',
    }
  },

  async load() {
    this.setData({ loading: true })
    try {
      const b: Balance = await member.getBalance()
      this.setData({
        balanceYuan: yuan(b.balance_cents),
        giftYuan: yuan(b.gift_cents),
        totalYuan: yuan(b.balance_cents + b.gift_cents),
        loading: false,
      })
    } catch (e) {
      // getBalance 内部已降级 0/0, 此处理论不会进; 防御性兜底
      this.setData({
        balanceYuan: '0',
        giftYuan: '0',
        totalYuan: '0',
        loading: false,
      })
      const msg = e instanceof ApiError ? e.message : '加载失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  onTapRecharge() {
    // C 端储值充值接口未就绪 (CONTRACT §16 待补), 占位
    wx.showToast({ title: '充值即将上线', icon: 'none' })
  },

  onTapLogin() {
    wx.navigateTo({ url: '/pages/join/join' })
  },
})
