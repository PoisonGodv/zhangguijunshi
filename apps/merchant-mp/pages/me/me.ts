// pages/me/me.ts — 「我的」页 (FE-MP-04)
//
// 数据流: onLoad / onShow / 下拉刷新 → 并发拉四路 B 端 BFF:
//   - getWhoami()              GET /v1/boss/whoami              → 门店/身份卡 (tenant_id + source)
//   - getDailySummaryToday()   GET /v1/boss/billing/summary/today → 今日营业额/订单数/客单价
//   - getSubscription()        GET /v1/boss/subscription        → 当前套餐 + 到期时间 + 状态
//   - getMemberCount()         GET /v1/boss/membership/members?page=1&size=1 → 会员总数 (取 total)
// 会员数: CONTRACT §24.7 路线 A,复用 §17.11 ListMembersPage 的 total 计数,后端 0 改动。
//
// 金额: 后端 int64 分 → 元(整除显示整数,否则两位小数),与 today/task-detail 一致。
// 失败策略: 单卡片失败只 toast,不阻塞其他卡片渲染。

import {
  getWhoami,
  getDailySummaryToday,
  getSubscription,
  getMemberCount,
  getAccessToken,
  clearTokens,
  logoutReq,
  ApiError,
} from '../../utils/request'
import type { Whoami, DailySummary, SubscriptionResp } from '../../types/api'

// === plan → 中文名 ===
const PLAN_LABEL: Record<string, string> = {
  free: '免费版',
  pro: 'Pro 版',
  flagship: '旗舰版',
}

// === subscription.status → 中文 ===
const SUB_STATUS_LABEL: Record<string, string> = {
  active: '生效中',
  expired: '已过期',
  cancelled: '已取消',
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
  // 订阅卡
  planLabel: string
  planStatus: string
  planExpire: string // 到期时间(free/无记录 → 永久)
  planRaw: string // free|pro|flagship (用于判断是否显示升级入口)
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
    planLabel: '免费版',
    planStatus: '',
    planExpire: '永久',
    planRaw: 'free',
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
    // 四路并发,单路失败只 toast,不阻塞其他卡片
    const [whoamiRes, summaryRes, subRes, memberCountRes] = await Promise.allSettled([
      getWhoami(),
      getDailySummaryToday(),
      getSubscription(),
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

    if (subRes.status === 'fulfilled') {
      this.applySubscription(subRes.value)
    } else {
      toastErr(subRes.reason, '订阅状态加载失败')
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

  applySubscription(r: SubscriptionResp): void {
    const plan = (r && r.plan) || 'free'
    const sub = r && r.subscription
    let statusText = ''
    let expireText = '永久'
    if (sub) {
      statusText = SUB_STATUS_LABEL[sub.Status] || sub.Status || ''
      if (sub.PeriodEndsAt) {
        expireText = formatDate(sub.PeriodEndsAt)
      }
    }
    this.setData({
      planRaw: plan,
      planLabel: PLAN_LABEL[plan] || plan,
      planStatus: statusText,
      planExpire: expireText,
    })
  },

  // === 静态功能区 ===

  onTapUpgrade(): void {
    wx.showToast({ title: '敬请期待', icon: 'none' })
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

/** ISO/RFC3339 → YYYY-MM-DD(后端 PeriodEndsAt 为 time.Time 序列化) */
function formatDate(iso: string): string {
  if (!iso) return ''
  // 直接截前 10 位,RFC3339 形如 2026-08-01T15:04:05+08:00
  return iso.length >= 10 ? iso.slice(0, 10) : iso
}

/** 单卡片失败: toast 中文,不阻塞其他卡片 */
function toastErr(err: unknown, fallback: string): void {
  const msg = err instanceof ApiError ? err.message : fallback
  wx.showToast({ title: msg, icon: 'none' })
}
