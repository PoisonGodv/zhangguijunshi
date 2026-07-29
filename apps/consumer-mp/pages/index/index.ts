// pages/index/index.ts — C 端首页 (FE-MP-01 骨架, tabBar 页, 游客可看)
//
// 数据流:
//   onLoad / onPullDownRefresh
//     → 并发 listCoupons('unused') + listGroupBuys()
//       → coupons 取前 3 映射为 HomeBenefit 视图 (今日福利区)
//       → groupBuys 直接渲染 (拼团专区)
//     → 失败: wx.showToast({icon:'none'}), 页面进入 empty 态
//   userStore 订阅: 切换 hero 登录态 (未登录=领券入会 CTA / 已登录=欢迎回来+会员码入口)
//   onShow: 已登录时静默 getMyProfile() 刷新 member (余额/等级可能在别页变更)
// 事件:
//   onTapJoin        → /pages/join/join (领券入会转化页)
//   onTapMemberCode  → /pages/member-code/member-code
//   onTapBenefit     → 骨架期 toast「详情即将上线」
//   onTapGroupbuy    → 未登录引导入会; 已登录真实参团 (joinGroupBuy), 成功刷新列表
//   onTapStoreGroup  → 骨架期 toast「进群功能即将上线」
// 埋点: home_view (onShow 主动 event; onPageShow 由 app.ts 全局注入, 不重复)

import { userStore, setUserMember } from '../../stores/user'
import { tenantStore } from '../../stores/tenant'
import { listCoupons } from '../../services/coupon'
import { listGroupBuys, joinGroupBuy } from '../../services/group-buy'
import { getMyProfile } from '../../services/member'
import { tracker } from '../../utils/tracker'
import { yuan } from '../../utils/format'
import { ApiError } from '../../utils/request'
import type { HomeBenefit, GroupBuyBrief, Store } from '../../types/api'

interface IndexData {
  loading: boolean
  isLoggedIn: boolean
  levelName: string
  storeName: string
  benefits: HomeBenefit[]
  groupbuys: Array<GroupBuyBrief & { priceYuan: string; originalYuan: string }>
  store: Store
}

// 骨架期门店 mock (后端门店接口未就绪, FE-MP-01 先写死)
const MOCK_STORE: Store = {
  id: 1,
  name: '掌柜优选·幸福里店',
  address: '幸福里社区商业街 12 号',
  distance_text: '距您 380m',
}

Page({
  data: {
    loading: true,
    isLoggedIn: false,
    levelName: '游客',
    storeName: '',
    benefits: [] as HomeBenefit[],
    groupbuys: [] as IndexData['groupbuys'],
    store: MOCK_STORE,
  } as IndexData,

  _unsub: null as null | (() => void),

  onLoad() {
    // 订阅 userStore: 登录态变化实时切换 hero
    this.setData(this._snap(userStore.get()))
    this._unsub = userStore.subscribe((s) => this.setData(this._snap(s)))

    const t = tenantStore.get()
    this.setData({ storeName: t.storeName || MOCK_STORE.name })

    this.load()
  },

  onUnload() {
    this._unsub && this._unsub()
  },

  onShow() {
    tracker.event('home_view')
    // 已登录: 静默刷新 member (骨架期容错, 失败不打扰用户)
    if (userStore.get().accessToken) {
      getMyProfile()
        .then((m) => setUserMember(m))
        .catch(() => {})
    }
  },

  onPullDownRefresh() {
    this.load().finally(() => wx.stopPullDownRefresh())
  },

  async load(): Promise<void> {
    this.setData({ loading: true })
    try {
      const [coupons, groupbuys] = await Promise.all([
        listCoupons('unused'),
        listGroupBuys(),
      ])
      const benefits: HomeBenefit[] = coupons.slice(0, 3).map((c) => ({
        id: c.id,
        tag: c.type === 'cash' ? '代金券' : c.type === 'discount' ? '折扣券' : '礼品券',
        title: c.title,
        desc: [c.threshold_text, c.expire_text].filter(Boolean).join(' · ') || c.value_text,
      }))
      this.setData({
        loading: false,
        benefits,
        groupbuys: groupbuys.map((g) => ({
          ...g,
          priceYuan: yuan(g.price_cents),
          originalYuan: yuan(g.original_cents),
        })),
      })
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '加载失败,下拉重试'
      this.setData({ loading: false, benefits: [], groupbuys: [] })
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  onTapJoin() {
    tracker.event('home_tap_join')
    wx.navigateTo({ url: '/pages/join/join' })
  },

  onTapMemberCode() {
    wx.navigateTo({ url: '/pages/member-code/member-code' })
  },

  onTapBenefit() {
    wx.showToast({ title: '详情即将上线', icon: 'none' })
  },

  async onTapGroupbuy(e: { currentTarget: { dataset: { id?: number; index?: number } } }) {
    // 从 dataset 取拼团 id (wxml 需带 data-id="{{item.id}}"); 未带则回退按 index 取
    let id = e.currentTarget.dataset.id
    if (!id && typeof e.currentTarget.dataset.index === 'number') {
      const item = this.data.groupbuys[e.currentTarget.dataset.index]
      id = item && item.id
    }
    if (!id) {
      wx.showToast({ title: '拼团不存在', icon: 'none' })
      return
    }
    // 未登录 → 引导入会 (参团需 JWT)
    if (!userStore.get().accessToken) {
      wx.showToast({ title: '请先领券入会再参团', icon: 'none' })
      wx.navigateTo({ url: '/pages/join/join' })
      return
    }
    try {
      wx.showLoading({ title: '参团中...', mask: true })
      await joinGroupBuy(id)
      wx.hideLoading()
      wx.showToast({ title: '参团成功', icon: 'success' })
      tracker.event('groupbuy_join_success')
      this.load() // 刷新进度
    } catch (err) {
      wx.hideLoading()
      // ERR_CODE_MSG 已映射 60013 满员/60015 已参团; 60014 拼团已结束用后端 message
      const msg = err instanceof ApiError ? err.message : '参团失败,请重试'
      wx.showToast({ title: msg, icon: 'none' })
    }
  },

  onTapStoreGroup() {
    wx.showToast({ title: '进群功能即将上线', icon: 'none' })
  },

  // userStore → 页面快照 (范式要求的 _snap)
  _snap(s: ReturnType<typeof userStore.get>) {
    return {
      isLoggedIn: !!s.accessToken,
      levelName: s.member?.level_name || '游客',
    }
  },
})
