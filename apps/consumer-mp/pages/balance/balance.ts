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
    // 充值 Simulate 面板 (CONTRACT §24.4)
    rechargeOpen: false,
    amountText: '',
    submitting: false,
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
    // 充值 Simulate 接口已就绪 (POST /v1/wxshop/member/balance/recharge, CONTRACT §24.4)
    // dev/test 便利: 直接入账, 不走 wx.requestPayment (真实 wxpay 延后到支付联调)
    this.setData({ rechargeOpen: true, amountText: '' })
  },

  onTapPreset(e: WechatMiniprogram.TouchEvent) {
    const amt = e.currentTarget.dataset.amt
    this.setData({ amountText: String(amt) })
  },

  onAmountInput(e: WechatMiniprogram.Input) {
    this.setData({ amountText: e.detail.value })
  },

  onTapCancelRecharge() {
    this.setData({ rechargeOpen: false, amountText: '' })
  },

  async onConfirmRecharge() {
    if (this.data.submitting) return
    const raw = (this.data.amountText || '').trim()
    if (!raw) {
      wx.showToast({ title: '请输入充值金额', icon: 'none' })
      return
    }
    const yuanVal = parseFloat(raw)
    if (isNaN(yuanVal) || yuanVal <= 0) {
      wx.showToast({ title: '请输入正确的充值金额', icon: 'none' })
      return
    }
    const amountCents = Math.round(yuanVal * 100)
    if (amountCents <= 0) {
      wx.showToast({ title: '充值金额需大于 0', icon: 'none' })
      return
    }
    this.setData({ submitting: true })
    try {
      await member.recharge(amountCents)
      wx.showToast({ title: '充值成功', icon: 'success' })
      this.setData({ submitting: false, rechargeOpen: false, amountText: '' })
      this.load() // 刷新余额
    } catch (err) {
      this.setData({ submitting: false })
      // 60003 超上限 / 40404 会员不存在 已映射中文; 其余用后端 message
      const msg = err instanceof ApiError ? err.message : '充值失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  onTapLogin() {
    wx.navigateTo({ url: '/pages/join/join' })
  },
})
