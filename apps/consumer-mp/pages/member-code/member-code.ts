// pages/member-code/member-code.ts — 会员码出示页(已登录)
import { userStore } from '../../stores/user'
import * as member from '../../services/member'
import { tracker } from '../../utils/tracker'

Page({
  data: {
    isLoggedIn: false,
    levelName: '游客',
    points: 0,
    nonce: '',
    countdown: 0,
  },

  _unsub: null as null | (() => void),
  _timer: null as null | number,

  onLoad() {
    this.setData(this._snap(userStore.get()))
    this._unsub = userStore.subscribe((s) => this.setData(this._snap(s)))

    if (!userStore.get().accessToken) {
      wx.redirectTo({ url: '/pages/join/join' })
      return
    }
    this.refreshQr()
    this._timer = setInterval(() => this.tick(), 1000) as unknown as number
  },

  onShow() {
    wx.onUserCaptureScreen(() => {
      wx.showToast({ title: '请勿截图,避免会员码泄露', icon: 'none' })
    })
  },

  onUnload() {
    if (this._timer) clearInterval(this._timer)
    wx.offUserCaptureScreen()
    if (this._unsub) this._unsub()
  },

  _snap(s: ReturnType<typeof userStore.get>) {
    return {
      isLoggedIn: !!s.accessToken,
      levelName: s.member?.level_name || '会员',
      points: s.member?.points || 0,
    }
  },

  async refreshQr() {
    try {
      // 后端返回 {code, expire_seconds:60} (相对秒数,非绝对时间戳); 倒计时直接取该值
      const r = await member.genMemberCode()
      this.setData({ nonce: r.code, countdown: r.expire_seconds })
      tracker.event('member_code_refresh')
    } catch {
      wx.showToast({ title: '获取会员码失败,请重试', icon: 'none' })
    }
  },

  tick() {
    // 相对秒数倒计时: 每秒减 1,到 0 自动换新码
    const left = Math.max(0, this.data.countdown - 1)
    this.setData({ countdown: left })
    if (left <= 0) this.refreshQr()
  },

  onTapRefresh() {
    this.refreshQr()
  },

  onTapLogin() {
    wx.redirectTo({ url: '/pages/join/join' })
  },
})
