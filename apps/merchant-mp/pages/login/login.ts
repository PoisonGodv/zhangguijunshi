// pages/login/login.ts — 老板登录页 (FE-MP-05 / CONTRACT §19 真登录)
//
// 数据流: 表单(租户ID + 手机号 + 密码) → bossLogin(tenantID, phone, password)
//         (POST /v1/boss/auth/login, 公开端点) → 成功存双 token → switchTab 到 today
//         失败 → toast ApiError.message (后端统一 ErrLoginFailed,不区分账号/密码)
//
// 守卫职责: 已有 access token 直接 switchTab 进 today,不闪登录表单。
// 出现时机: 仅「无 token 或 refresh 也失败」时由业务页 onLoad 守卫 redirectTo 进来。
//
// 安全约束 (ARCH-01): tenant/store/role 永远由后端从 JWT ctx 解析;
//   这里的租户ID仅作为登录入参,登录成功后前端不保存/不信任任何租户信息。

import { bossLogin, getAccessToken, ApiError } from '../../utils/request'

interface LoginData {
  tenantId: string // 租户ID (MVP 单租户期默认 1,字段保留)
  phone: string
  password: string
  loading: boolean
}

Page({
  data: {
    tenantId: '1',
    phone: '',
    password: '',
    loading: false,
  } as LoginData,

  onLoad() {
    // 有有效 token 直接进 today,不闪登录表单
    if (getAccessToken()) {
      wx.switchTab({ url: '/pages/today/today' })
    }
  },

  onTenantInput(e: WechatMiniprogram.Input) {
    this.setData({ tenantId: (e.detail.value || '').trim() })
  },

  onPhoneInput(e: WechatMiniprogram.Input) {
    this.setData({ phone: (e.detail.value || '').trim() })
  },

  onPasswordInput(e: WechatMiniprogram.Input) {
    this.setData({ password: e.detail.value || '' })
  },

  async onSubmit(): Promise<void> {
    if (this.data.loading) return // 防重复提交

    const { tenantId, phone, password } = this.data
    // 简单校验: 三项非空 + 手机号 11 位
    if (!tenantId || !phone || !password) {
      wx.showToast({ title: '请填写完整登录信息', icon: 'none' })
      return
    }
    if (!/^1\d{10}$/.test(phone)) {
      wx.showToast({ title: '请输入 11 位手机号', icon: 'none' })
      return
    }
    const tid = Number(tenantId)
    if (!tid || tid <= 0) {
      wx.showToast({ title: '租户ID不正确', icon: 'none' })
      return
    }

    this.setData({ loading: true })
    try {
      await bossLogin(tid, phone, password)
      wx.switchTab({ url: '/pages/today/today' })
    } catch (err) {
      const msg = err instanceof ApiError ? err.message : '登录失败'
      wx.showToast({ title: msg, icon: 'none' })
      this.setData({ loading: false })
    }
  },
})
