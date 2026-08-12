// pages/recharge/recharge.ts — 店员手动充值页 (MVP-3, CONTRACT §25.3)
//
// 数据流:
//   顶部搜索框 input → 300ms debounce → searchMembers(q) (GET /v1/boss/membership/members?q=&page=1&size=20)
//   点选会员 → 进入「金额」态,展示预设档位 + 自定义输入
//   预设档位 (充200送20 / 充500送80 / 充1000送200) 仅作 amount 预设,赠送文案仅展示,
//     实际赠送以后端 GiftForAmount 返回 (RechargeResult.GiftedCents) 为准,前端绝不传 gift。
//   自定义金额: 元 → 分 (×100)。
//   「确认充值」→ showModal 二次确认 (会员名+金额) → rechargeMember(id, amountCents)
//     (POST /v1/boss/membership/members/:id/recharge {amount_cents}) → 回执 (实付+赠送+新余额)。
//   失败 toast errcode 中文 (60003 超单店上限 / 42302 冻结 / 40402 不存在)。
//
// 后端字段说明 (MVP-3 已读源码确认):
//   memberDTO (handler/membership.go) 有 json tag → snake_case (id/name/level_name/balance_cents/...)。
//   membership.RechargeResult 无 json tag → PascalCase (BalanceCents/GiftCents/RechargedCents/GiftedCents)。
//   Recharge body 有 json tag → snake_case: {amount_cents} (binding min=1, 0/负后端拒 40001)。
//
// 金额: 后端 int64 分 → 元 (整除显示整数,否则两位小数),与 me/billing/today 一致。
// ARCH-01: tenant/store/operator 永远由后端从 JWT ctx 解析,前端只传 member_id 路径参数 + amount_cents。

import {
  searchMembers,
  rechargeMember,
  getAccessToken,
  ApiError,
} from '../../utils/request'
import type { MemberDTO, RechargeResult } from '../../types/api'

// === 本页参数 ===
const SEARCH_DEBOUNCE_MS = 300

/** 预设档位 (仅作 amount 预设;giftLabel 仅展示,实际赠送以后端为准) */
interface Preset {
  cents: number
  yuan: string
  giftLabel: string
}
const PRESETS: Preset[] = [
  { cents: 20000, yuan: '200', giftLabel: '送 ¥20' },
  { cents: 50000, yuan: '500', giftLabel: '送 ¥80' },
  { cents: 100000, yuan: '1000', giftLabel: '送 ¥200' },
]

/** 会员行视图模型 (MemberDTO + 余额元显示) */
interface MemberRow extends MemberDTO {
  balance_yuan: string
}

/** 已选会员视图模型 */
interface SelectedMember {
  id: number
  name: string
  level_name: string
  balance_yuan: string
}

/** 回执视图模型 */
interface ReceiptVM {
  member_name: string
  amount_yuan: string // 本次实付
  gift_yuan: string // 本次赠送
  balance_yuan: string // 入账后实付余额
  gift_balance_yuan: string // 入账后赠送余额
  total_yuan: string // 合计可用 (实付+赠送)
}

interface RechargeData {
  keyword: string
  members: MemberRow[]
  listLoading: boolean
  searched: boolean // 是否已搜过 (区分初始引导与「未找到」)
  member: SelectedMember | null
  presets: Preset[]
  amountCents: number // 当前选中/输入的金额 (分)
  amountYuan: string // 底部合计显示
  customYuan: string // 自定义输入框 (元)
  submitting: boolean
  receipt: ReceiptVM | null
}

Page({
  data: {
    keyword: '',
    members: [] as MemberRow[],
    listLoading: false,
    searched: false,
    member: null,
    presets: PRESETS,
    amountCents: 0,
    amountYuan: '0',
    customYuan: '',
    submitting: false,
    receipt: null,
  } as RechargeData,

  // === 内部态 (非 data, 避免触发 setData) ===
  _searchTimer: 0 as unknown as ReturnType<typeof setTimeout>,

  onLoad() {
    // 登录守卫 (FE-MP-05): 无 token 或 refresh 也失败 → 去登录页
    if (!getAccessToken()) {
      wx.redirectTo({ url: '/pages/login/login' })
      return
    }
  },

  // === 搜索输入 (300ms debounce) ===
  onKeywordInput(e: { detail: { value: string } }) {
    const keyword = (e.detail && e.detail.value) || ''
    this.setData({ keyword })
    if (this._searchTimer) clearTimeout(this._searchTimer)
    this._searchTimer = setTimeout(() => {
      this.doSearch()
    }, SEARCH_DEBOUNCE_MS)
  },

  onKeywordClear() {
    this.setData({ keyword: '', members: [], searched: false })
    if (this._searchTimer) clearTimeout(this._searchTimer)
  },

  // === 执行会员搜索 ===
  async doSearch(): Promise<void> {
    const d = this.data as RechargeData
    const q = (d.keyword || '').trim()
    if (!q) {
      this.setData({ members: [], searched: false })
      return
    }
    this.setData({ listLoading: true })
    try {
      const res = await searchMembers(q, 1, 20)
      const items = (res && res.items) || []
      const rows: MemberRow[] = items.map((m) => ({ ...m, balance_yuan: yuan(m.balance_cents) }))
      this.setData({ members: rows, searched: true })
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '搜索会员失败'
      wx.showToast({ title: msg, icon: 'none' })
    } finally {
      this.setData({ listLoading: false })
    }
  },

  // === 点选会员 → 进入金额态 ===
  onSelectMember(e: { currentTarget: { dataset: Record<string, unknown> } }) {
    const id = Number(e.currentTarget.dataset.id)
    if (!id) return
    const d = this.data as RechargeData
    const m = (d.members || []).find((x) => x.id === id)
    if (!m) return
    if (m.block) {
      wx.showToast({ title: '该会员已冻结', icon: 'none' })
      return
    }
    this.setData({
      member: { id: m.id, name: m.name || `会员 ${m.id}`, level_name: m.level_name, balance_yuan: m.balance_yuan },
      // 重置金额,默认选中第一档
      amountCents: PRESETS[0].cents,
      amountYuan: yuan(PRESETS[0].cents),
      customYuan: '',
    })
  },

  // === 更换会员 → 回搜索态 ===
  onClearMember() {
    this.setData({ member: null, amountCents: 0, amountYuan: '0', customYuan: '' })
  },

  // === 预设档位 ===
  onPreset(e: { currentTarget: { dataset: Record<string, unknown> } }) {
    const cents = Number(e.currentTarget.dataset.cents)
    if (!cents) return
    this.setData({ amountCents: cents, amountYuan: yuan(cents), customYuan: '' })
  },

  // === 自定义金额 (元 → 分) ===
  onCustomInput(e: { detail: { value: string } }) {
    const raw = (e.detail && e.detail.value) || ''
    this.setData({ customYuan: raw })
    const cents = parseYuanToCents(raw)
    this.setData({ amountCents: cents, amountYuan: yuan(cents) })
  },

  // === 确认充值 → 二次确认 → 调接口 → 回执 ===
  onConfirm(): void {
    const d = this.data as RechargeData
    if (!d.member || !d.amountCents || d.amountCents <= 0 || d.submitting) return
    const member = d.member
    const amountCents = d.amountCents
    wx.showModal({
      title: '确认充值',
      content: `为 ${member.name} 充值 ¥${yuan(amountCents)}?`,
      confirmText: '确认充值',
      confirmColor: '#FF6B35',
      success: (r) => {
        if (!r.confirm) return
        this.doRecharge(member, amountCents)
      },
    })
  },

  // === 内部: 执行充值 ===
  async doRecharge(member: SelectedMember, amountCents: number): Promise<void> {
    this.setData({ submitting: true })
    try {
      const res = await rechargeMember(member.id, amountCents)
      this.setData({ receipt: buildReceipt(member, res) })
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '充值失败'
      wx.showToast({ title: msg, icon: 'none' })
    } finally {
      this.setData({ submitting: false })
    }
  },

  // === 回执「再充一笔」 → 回搜索态 ===
  onNewRecharge() {
    this.setData({
      receipt: null,
      member: null,
      members: [],
      keyword: '',
      searched: false,
      amountCents: 0,
      amountYuan: '0',
      customYuan: '',
    })
  },
})

/** 后端金额 int64 分 → 元(整除显示整数,否则两位小数),与 me/billing/today 对齐 */
function yuan(cents: number | string): string {
  const v = Number(cents) / 100
  return v % 1 === 0 ? String(v) : v.toFixed(2)
}

/** 自定义输入 元字符串 → 分 (两位小数;非法/空/负 → 0) */
function parseYuanToCents(raw: string): number {
  const s = (raw || '').trim()
  if (!s) return 0
  const n = Number(s)
  if (!isFinite(n) || n <= 0) return 0
  return Math.round(n * 100)
}

/** 构造回执视图模型 (实付+赠送+新余额) */
function buildReceipt(member: SelectedMember, r: RechargeResult): ReceiptVM {
  const total = Number(r.BalanceCents || 0) + Number(r.GiftCents || 0)
  return {
    member_name: member.name,
    amount_yuan: yuan(r.RechargedCents || 0),
    gift_yuan: yuan(r.GiftedCents || 0),
    balance_yuan: yuan(r.BalanceCents || 0),
    gift_balance_yuan: yuan(r.GiftCents || 0),
    total_yuan: yuan(total),
  }
}
