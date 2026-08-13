// pages/me/me.ts — 「我的」页 (FE-MP-04)
//
// 数据流: onLoad / onShow / 下拉刷新 → 并发拉三路 B 端 BFF:
//   - getWhoami()              GET /v1/boss/whoami              → 门店/身份卡 (tenant_id + source)
//   - getDailySummaryToday()   GET /v1/boss/billing/summary/today → 今日营业额/订单数/客单价
//   - getMemberCount()         GET /v1/boss/membership/members?page=1&size=1 → 会员总数 (取 total)
// 会员数: CONTRACT §24.7 路线 A,复用 §17.11 ListMembersPage 的 total 计数,后端 0 改动。
// 订阅卡: CONTRACT §25.2 — MVP 砍在线支付/订阅,GET /v1/boss/subscription 已下架(404)。
//   本页不再调用 getSubscription,订阅卡静态展示「MVP 免费版 · 全部功能开放」。
//   SubscriptionResp 类型/常量保留待用(V1 恢复订阅时再启用)。
//
// 金额: 后端 int64 分 → 元(整除显示整数,否则两位小数),与 today/task-detail 一致。
// 失败策略: 单卡片失败只 toast,不阻塞其他卡片渲染。

import {
  getWhoami,
  getDailySummaryToday,
  getMemberCount,
  getAccessToken,
  clearTokens,
  logoutReq,
  ApiError,
} from '../../utils/request'
import type { Whoami, DailySummary } from '../../types/api'

// === plan → 中文名 === (§25.2: MVP 静态免费版,保留常量待 V1 恢复订阅时启用)
const PLAN_LABEL: Record<string, string> = {
  free: '免费版',
  pro: 'Pro 版',
  flagship: '旗舰版',
}

// === whoami.source → 中文 ===
const SOURCE_LABEL: Record<string, string> = {
  merchant: '商家端',
  consumer: '消费者端',
  admin: '运营端',
  system: '系统',
  unresolved: '未识别',
}

interface MeData {
  loading: boolean
  // 门店卡
  tenantId: number
  sourceLabel: string
  // 今日经营摘要 (单位已换算成元 / 单数 / 元/单)
  summaryDate: string
  revenueYuan: string
  orderCount: number
  avgYuan: string // 客单价(0 单显示 —)
  memberCountText: string // 会员数(§24.7: /v1/boss/membership/members total,失败/加载中保持 —)
  // 订阅卡 (§25.2: MVP 静态免费版,不拉接口,plan 恒 free)
  planLabel: string
}

Page({
  data: {
    loading: true,
    tenantId: 0,
    sourceLabel: '商家端',
    summaryDate: '',
    revenueYuan: '0',
    orderCount: 0,
    avgYuan: '—',
    memberCountText: '—',
    planLabel: PLAN_LABEL.free,
  } as MeData,

  onLoad() {
    // 登录守卫 (FE-MP-05): 无 token 或 refresh 也失败 → 去登录页
    if (!getAccessToken()) {
      wx.redirectTo({ url: '/pages/login/login' })
      return
    }
    this.load()
  },

  onShow() {
    // 登录守卫: 退出后 reLaunch 回登录页,若从后台/栈残留回到本页且已无 token,同样拦下
    if (!getAccessToken()) {
      wx.redirectTo({ url: '/pages/login/login' })
      return
    }
    // tab 切回时刷新一次,让营业额/订阅状态保持最新
    this.load()
  },

  onPullDownRefresh() {
    this.load().finally(() => wx.stopPullDownRefresh())
  },

  async load(): Promise<void> {
    this.setData({ loading: true })
    // 三路并发,单路失败只 toast,不阻塞其他卡片 (§25.2: 订阅卡已静态化,不再请求)
    const [whoamiRes, summaryRes, memberCountRes] = await Promise.allSettled([
      getWhoami(),
      getDailySummaryToday(),
      getMemberCount(),
    ])

    if (whoamiRes.status === 'fulfilled') {
      this.applyWhoami(whoamiRes.value)
    } else {
      toastErr(whoamiRes.reason, '门店信息加载失败')
    }

    if (summaryRes.status === 'fulfilled') {
      this.applySummary(summaryRes.value)
    } else {
      toastErr(summaryRes.reason, '经营摘要加载失败')
    }

    if (memberCountRes.status === 'fulfilled') {
      this.applyMemberCount(memberCountRes.value)
    } else {
      toastErr(memberCountRes.reason, '会员数加载失败')
    }

    this.setData({ loading: false })
  },

  applyWhoami(w: Whoami): void {
    this.setData({
      tenantId: w.tenant_id || 0,
      sourceLabel: SOURCE_LABEL[w.source] || '商家端',
    })
  },

  applySummary(s: DailySummary): void {
    const count = s.OrderCount || 0
    // 营业额以实收 (NetCents) 为准;客单价 = 营业额 / 单数,0 单显示 —
    const revenue = s.NetCents || 0
    const avg = count > 0 ? Math.round(revenue / count) : 0
    this.setData({
      summaryDate: s.Date || '',
      revenueYuan: yuan(revenue),
      orderCount: count,
      avgYuan: count > 0 ? yuan(avg) : '—',
    })
  },

  /** 会员数卡片 (§24.7): ListMembersPage total → 文本;失败时不调用,保持初始 '—' */
  applyMemberCount(total: number): void {
    this.setData({ memberCountText: String(total) })
  },

  // === 静态功能区 ===

  onTapRecharge(): void {
    // MVP-3: 店员手动充值页 (CONTRACT §25.3)
    wx.navigateTo({ url: '/pages/recharge/recharge' })
  },

  onTapRedeem(): void {
    // MVP-4: 会员码核销页 (扣储值/积积分)
    wx.navigateTo({ url: '/pages/redeem/redeem' })
  },

  onTapSettings(): void {
    wx.showToast({ title: '设置 · 敬请期待', icon: 'none' })
  },

  onTapHelp(): void {
    wx.showToast({ title: '帮助 · 敬请期待', icon: 'none' })
  },

  onTapAbout(): void {
    wx.showModal({
      title: '掌柜军师',
      content: '三四线个体零售 AI 营销军师\n商家端小程序 (M3)',
      showCancel: false,
      confirmText: '知道了',
    })
  },

  onTapLogout(): void {
    // FE-MP-05: showModal 确认 → logoutReq() (POST /v1/boss/auth/logout,本地失败也继续)
    //   → 无论结果如何 clearTokens + reLaunch 到登录页
    wx.showModal({
      title: '退出登录',
      content: '确认退出当前账号?',
      confirmText: '退出',
      confirmColor: '#FF6B35',
      success: (res) => {
        if (!res.confirm) return
        logoutReq()
          .catch(() => {
            // 本地/网络失败也继续清 token 退出
          })
          .finally(() => {
            clearTokens()
            wx.reLaunch({ url: '/pages/login/login' })
          })
      },
    })
  },
})

/** 后端金额 int64 分 → 元(整除显示整数,否则两位小数),与 today/task-detail 对齐 */
function yuan(cents: number | string): string {
  const v = Number(cents) / 100
  return v % 1 === 0 ? String(v) : v.toFixed(2)
}

/** 单卡片失败: toast 中文,不阻塞其他卡片 */
function toastErr(err: unknown, fallback: string): void {
  const msg = err instanceof ApiError ? err.message : fallback
  wx.showToast({ title: msg, icon: 'none' })
}
