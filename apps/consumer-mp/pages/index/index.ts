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
//   onTapBenefit     → 跳券详情 (福利卡=真实券, GET /v1/wxshop/coupons/:id); 无 id 兜底 toast
//   onTapGroupbuy    → 跳拼团列表/详情 (参团在详情页完成; 仅 1 个直跳详情)
//   onTapStoreGroup  → /pages/stores/stores (进群二维码在门店页展示)
// 埋点: home_view (onShow 主动 event; onPageShow 由 app.ts 全局注入, 不重复)
// 门店: M8-C 接通真实 GET /v1/wxshop/stores (services/store.listStores), 取列表首店渲染
//       "附近门店"卡; 游客/未装配门店时 service 降级空数组 → 卡片隐藏 (不写死 mock)。

import { userStore, setUserMember } from '../../stores/user'
import { tenantStore } from '../../stores/tenant'
import { listCoupons } from '../../services/coupon'
import { listGroupBuys } from '../../services/group-buy'
import { getMyProfile } from '../../services/member'
import { listStores } from '../../services/store'
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
  store: Store | null
}

Page({
  data: {
    loading: true,
    isLoggedIn: false,
    levelName: '游客',
    storeName: '',
    benefits: [] as HomeBenefit[],
    groupbuys: [] as IndexData['groupbuys'],
    store: null as Store | null,
  } as IndexData,

  _unsub: null as null | (() => void),

  onLoad() {
    // 订阅 userStore: 登录态变化实时切换 hero
    this.setData(this._snap(userStore.get()))
    this._unsub = userStore.subscribe((s) => this.setData(this._snap(s)))

    const t = tenantStore.get()
    // storeName 优先取 tenantStore (进店扫码分流后写入); 真实门店名在 load() 拉到后回填
    this.setData({ storeName: t.storeName || '' })

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
      const [coupons, groupbuys, stores] = await Promise.all([
        listCoupons('unused'),
        listGroupBuys(),
        listStores(),
      ])
      const benefits: HomeBenefit[] = coupons.slice(0, 3).map((c) => ({
        id: c.id,
        tag: c.type === 'cash' ? '代金券' : c.type === 'discount' ? '折扣券' : '礼品券',
        title: c.title,
        desc: [c.threshold_text, c.expire_text].filter(Boolean).join(' · ') || c.value_text,
      }))
      // 附近门店: 取真实门店列表首店; 空列表 → store=null (wxml 隐藏卡片)
      const store = stores.length ? stores[0] : null
      const t = tenantStore.get()
      this.setData({
        loading: false,
        benefits,
        groupbuys: groupbuys.map((g) => ({
          ...g,
          priceYuan: yuan(g.price_cents),
          originalYuan: yuan(g.original_cents),
        })),
        store,
        // storeName: tenantStore 优先 (进店分流), 否则回填真实首店名
        storeName: t.storeName || (store ? store.name : ''),
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

  onTapBenefit(e: { currentTarget: { dataset: { id?: number } } }) {
    // 今日福利卡 = 真实券 (listCoupons 映射, data-id=券 id) → 跳券详情 (GET /v1/wxshop/coupons/:id)
    const id = e.currentTarget.dataset.id
    if (!id) {
      wx.showToast({ title: '详情即将上线', icon: 'none' })
      return
    }
    wx.navigateTo({ url: `/pages/coupon-detail/coupon-detail?id=${id}` })
  },

  onTapGroupbuy(e: { currentTarget: { dataset: { id?: number; index?: number } } }) {
    // 从 dataset 取拼团 id (wxml 需带 data-id="{{item.id}}"); 未带则回退按 index 取
    let id = e.currentTarget.dataset.id
    if (!id && typeof e.currentTarget.dataset.index === 'number') {
      const item = this.data.groupbuys[e.currentTarget.dataset.index]
      id = item && item.id
    }
    // 拼团入口: 优先列表页; 仅 1 个拼团时直跳详情
    if (this.data.groupbuys.length === 1 && id) {
      wx.navigateTo({ url: `/pages/group-buy-detail/group-buy-detail?id=${id}` })
      return
    }
    wx.navigateTo({ url: '/pages/group-buy/group-buy' })
  },

  onTapStoreGroup() {
    // 进群入口: 优先跳门店列表页 (本期 group_qr 在门店页展示)
    wx.navigateTo({ url: '/pages/stores/stores' })
  },

  // userStore → 页面快照 (范式要求的 _snap)
  _snap(s: ReturnType<typeof userStore.get>) {
    return {
      isLoggedIn: !!s.accessToken,
      levelName: s.member?.level_name || '游客',
    }
  },
})
