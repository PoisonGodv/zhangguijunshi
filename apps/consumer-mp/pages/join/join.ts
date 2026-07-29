// pages/join/join.ts — 领券入会转化页 (FE-MP-01 骨架, C 端核心转化链路)
//
// 转化漏斗 (tracker.event):
//   join_view        — 页面曝光 (onLoad)
//   join_phone_auth  — 用户点击手机号授权按钮且授权成功拿到 e.detail.code
//   join_success     — 全链路完成: login → profile → claimNewMemberCoupon
//
// 全链路 (onGetPhone):
//   1. agreed 校验 → 未同意 toast 拦截
//   2. e.detail.code 校验 → 用户拒绝/旧库 toast 拦截
//   3. tracker.event('join_phone_auth'), submitting=true
//   4. wx.login → code  (Promise 包装)
//   5. auth.login(code, tenantID) → setUserTokens (真实后端)
//   6. member.getMyProfile() → setUserMember (真实后端; phone_masked 本地回填掩码,
//      后端恒空串) — 捕获档案原始手机号 rawPhone 供领券用
//   7. member.claimNewMemberCoupon(rawPhone) → 幂等(已领 already_claimed) →
//      tracker.event('join_success'); 领券失败独立 catch 不阻断入会主流程
//   8. wx.requestSubscribeMessage (骨架占位, 模板 ID 待申请填入; 失败不阻塞)
//   9. submitting=false, success=true → 成功态
//   catch: ApiError 中文 toast, submitting=false

import * as auth from '../../services/auth'
import * as member from '../../services/member'
import { setUserTokens, setUserMember } from '../../stores/user'
import { tracker } from '../../utils/tracker'
import { maskPhone } from '../../utils/format'
import { ApiError } from '../../utils/request'

interface JoinData {
  agreed: boolean
  submitting: boolean
  success: boolean
}

Page({
  data: {
    agreed: false,
    submitting: false,
    success: false,
  } as JoinData,

  onLoad() {
    // 漏斗第 1 环: 页面曝光
    tracker.event('join_view')
  },

  onToggleAgree() {
    this.setData({ agreed: !this.data.agreed })
  },

  async onGetPhone(e: { detail: { code?: string; phoneNumber?: string } }) {
    // 1. 协议未同意 → 拦截
    if (!this.data.agreed) {
      wx.showToast({ title: '请先同意协议', icon: 'none' })
      return
    }
    // 2. 用户拒绝授权 / 基础库过旧 → 拦截
    if (!e.detail.code) {
      wx.showToast({ title: '需要授权手机号才能领券', icon: 'none' })
      return
    }

    // 漏斗第 2 环: 手机号授权通过
    tracker.event('join_phone_auth')
    this.setData({ submitting: true })

    try {
      // 4. wx.login 取 code (Promise 包装)
      const code = await new Promise<string>((resolve, reject) => {
        wx.login({
          success: (res) => (res.code ? resolve(res.code) : reject(new Error('wx.login 无 code'))),
          fail: reject,
        })
      })

      // 5. 后端登录 → 存 token (真实)
      const resp = await auth.login(code, getApp().globalData.tenantID)
      setUserTokens(resp.access_token, resp.refresh_token)

      // 6. 拉会员档案 → 存 member (真实); 后端 phone_masked 恒空串(只存 hash),
      //    本地回填掩码展示; 捕获档案原始手机号供领券(后端 claim 接口要原始号,自己 hash)
      const rawPhone = e.detail.phoneNumber || ''
      const profile = await member.getMyProfile()
      if (!profile.phone_masked && rawPhone) {
        profile.phone_masked = maskPhone(rawPhone)
      }
      setUserMember(profile)

      // 7. 领新人券 (幂等: 已领后端返 already_claimed:true, 不报错)。领券失败独立
      //    catch: 入会是主流程, 领券失败只 toast 不阻断成功态。
      try {
        const r = await member.claimNewMemberCoupon(rawPhone)
        if (r.already_claimed) {
          wx.showToast({ title: '您已领取过新人券', icon: 'none' })
        }
        // 漏斗第 3 环: 全链路完成
        tracker.event('join_success')
      } catch (claimErr) {
        // 领券失败 (如券已发完 60005): 独立提示, 不阻断入会成功
        const cmsg = claimErr instanceof ApiError ? claimErr.message : '领券失败,请稍后在券包查看'
        wx.showToast({ title: cmsg, icon: 'none' })
      }

      // 8. 订阅消息 (骨架占位: tmplIds 待申请真实模板 ID 后填入; 失败不阻塞主流程)
      wx.requestSubscribeMessage({
        tmplIds: [], // TODO: 订阅消息模板 ID 待申请填入
        success: () => {},
        fail: () => {},
      })

      // 9. 成功态
      this.setData({ submitting: false, success: true })
    } catch (err) {
      const msg = err instanceof ApiError ? err.message : '领取失败请重试'
      wx.showToast({ title: msg, icon: 'none' })
      this.setData({ submitting: false })
    }
  },

  onTapMemberCode() {
    wx.navigateTo({ url: '/pages/member-code/member-code' })
  },

  onTapCoupons() {
    wx.switchTab({ url: '/pages/coupons/coupons' })
  },
})
