// pages/billing/billing.ts — 开单页 (FE-MP-02 核心)
//
// 数据流:
//   onLoad → loadProducts(true) (GET /v1/boss/catalog/products?limit=&offset=&query=) 第一页
//   onReachBottom → loadProducts(false) 加载下一页 (hasMore=false 停止)
//   搜索框 input → 300ms debounce → 重新走 loadProducts(true)
//   「扫码」→ wx.scanCode → getProductByBarcode → 命中直接入购物车
//   商品行「+」/ 购物车步进器 → 维护 cart (Map product_id → CartLine)
//   「开单」→ createPendingOrder({items:[...]}) → showModal 确认收款 → payOrder(id) → 回执
//   「再开一单」→ clearCart + 回执收起
//
// 后端字段说明 (M1-S1/M4-S1 已读源码确认):
//   catalog.Product / billing.Order 均无 json tag, Go encoding/json 默认 PascalCase 输出
//   (ID/Name/PriceCents/TotalCents/...), 与 interfaces/http/*_test.go 断言一致。
//   types/api.d.ts 中 Product/Order 按 PascalCase 声明,与本页消费一致。
//   CreateOrder body 有 json tag, 走 snake_case: {store_id?, member_id?, channel?, payment_method?, items:[{product_id, qty}]}
//   本页走 CreatePendingOrder + SimulatePay (M4-S1 已上线, 支持 pending→paid 状态机) 以匹配
//   「开单→收款→回执」的真实两段式 UX。两接口共用同一 createOrderRequest body。

import {
  listProducts,
  getProductByBarcode,
  createPendingOrder,
  payOrder,
  getAccessToken,
  ApiError,
} from '../../utils/request'
import type { Product, Order, CreateOrderItem } from '../../types/api'

// === 本页分页/搜索参数 ===
const PAGE_SIZE = 20
const SEARCH_DEBOUNCE_MS = 300

/** 商品行视图模型 (Product + 价格元显示) */
interface ProductRow extends Product {
  price_yuan: string
}

/** 购物车行视图模型 */
interface CartLine {
  product_id: number
  name: string
  unit: string
  price_cents: number
  price_yuan: string
  qty: number
  subtotal_cents: number
  subtotal_yuan: string
}

/** 回执视图模型 */
interface ReceiptVM {
  order_no: string
  total_yuan: string
  count: number
  paid_at: string
}

interface BillingData {
  keyword: string
  scanning: boolean
  products: ProductRow[]
  listLoading: boolean
  listFinished: boolean
  cart: CartLine[]
  totalCount: number
  totalYuan: string
  submitting: boolean
  receipt: ReceiptVM | null
}

Page({
  data: {
    keyword: '',
    scanning: false,
    products: [] as ProductRow[],
    listLoading: false,
    listFinished: false,
    cart: [] as CartLine[],
    totalCount: 0,
    totalYuan: '0',
    submitting: false,
    receipt: null,
  } as BillingData,

  // === 内部态 (非 data, 避免触发 setData) ===
  _offset: 0,
  _searchTimer: 0 as unknown as ReturnType<typeof setTimeout>,

  onLoad() {
    // 登录守卫 (FE-MP-05): 无 token 或 refresh 也失败 → 去登录页
    if (!getAccessToken()) {
      wx.redirectTo({ url: '/pages/login/login' })
      return
    }
    this._offset = 0
    this.refreshList()
  },

  onPullDownRefresh() {
    this.refreshList().finally(() => wx.stopPullDownRefresh())
  },

  onReachBottom() {
    const d = this.data as BillingData
    if (d.listLoading || d.listFinished) return
    this.loadMore()
  },

  // === 搜索输入 (300ms debounce) ===
  onKeywordInput(e: { detail: { value: string } }) {
    const keyword = (e.detail && e.detail.value) || ''
    this.setData({ keyword })
    if (this._searchTimer) clearTimeout(this._searchTimer)
    this._searchTimer = setTimeout(() => {
      this.refreshList()
    }, SEARCH_DEBOUNCE_MS)
  },

  onKeywordClear() {
    this.setData({ keyword: '' })
    if (this._searchTimer) clearTimeout(this._searchTimer)
    this.refreshList()
  },

  // === 刷新商品列表 (重置到第一页) ===
  async refreshList(): Promise<void> {
    this._offset = 0
    this.setData({ products: [], listFinished: false })
    await this.loadMore()
  },

  // === 加载下一页 ===
  async loadMore(): Promise<void> {
    const d = this.data as BillingData
    if (d.listLoading) return
    this.setData({ listLoading: true })
    try {
      const res = await listProducts(PAGE_SIZE, this._offset, d.keyword || undefined)
      const items = (res && res.items) || []
      // 仅展示 active 商品 (后端默认全量, archived 不允许开单)
      const rows: ProductRow[] = items
        .filter((p) => p.Status === 'active')
        .map((p) => ({ ...p, price_yuan: yuan(p.PriceCents) }))
      const merged = (d.products || []).concat(rows)
      this._offset += items.length
      this.setData({
        products: merged,
        listFinished: items.length < PAGE_SIZE,
      })
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '加载商品失败'
      wx.showToast({ title: msg, icon: 'none' })
    } finally {
      this.setData({ listLoading: false })
    }
  },

  // === 扫码 → 条码查商品 → 入购物车 ===
  onScan() {
    const d = this.data as BillingData
    if (d.scanning) return
    this.setData({ scanning: true })
    wx.scanCode({
      success: async (r) => {
        const barcode = ((r && r.result) || '').trim()
        if (!barcode) {
          wx.showToast({ title: '未识别条码', icon: 'none' })
          return
        }
        try {
          const p = await getProductByBarcode(barcode)
          if (!p || p.Status !== 'active') {
            wx.showToast({ title: '商品未上架', icon: 'none' })
            return
          }
          this.addToCart(p, 1)
          wx.showToast({ title: `已加入:${p.Name}`, icon: 'none' })
        } catch (e) {
          const msg = e instanceof ApiError ? e.message : '未找到该条码商品'
          wx.showToast({ title: msg, icon: 'none' })
        }
      },
      fail: () => {
        // 用户取消扫码不打扰
      },
      complete: () => {
        this.setData({ scanning: false })
      },
    })
  },

  // === 点商品行「+」→ 入购物车 ===
  onAddProduct(e: { currentTarget: { dataset: Record<string, unknown> } }) {
    const id = Number(e.currentTarget.dataset.id)
    if (!id) return
    const d = this.data as BillingData
    const p = (d.products || []).find((x) => x.ID === id)
    if (!p) return
    this.addToCart(p, 1)
  },

  // === 购物车数量 + ===
  onInc(e: { currentTarget: { dataset: Record<string, unknown> } }) {
    const id = Number(e.currentTarget.dataset.id)
    if (!id) return
    this.setCartQty(id, this.findQty(id) + 1)
  },

  // === 购物车数量 - (减到 0 即移除) ===
  onDec(e: { currentTarget: { dataset: Record<string, unknown> } }) {
    const id = Number(e.currentTarget.dataset.id)
    if (!id) return
    const q = this.findQty(id)
    this.setCartQty(id, q - 1)
  },

  // === 购物车行删除 ===
  onRemoveLine(e: { currentTarget: { dataset: Record<string, unknown> } }) {
    const id = Number(e.currentTarget.dataset.id)
    if (!id) return
    this.setCartQty(id, 0)
  },

  // === 清空购物车 ===
  onClearCart() {
    const d = this.data as BillingData
    if (!d.cart.length) return
    wx.showModal({
      title: '清空购物车',
      content: '确定清空已选商品?',
      confirmText: '清空',
      confirmColor: '#FF6B35',
      success: (r) => {
        if (r.confirm) this.applyCart([])
      },
    })
  },

  // === 开单 → 创建 pending → 弹「收款」 → SimulatePay → 回执 ===
  async onSubmit(): Promise<void> {
    const d = this.data as BillingData
    if (!d.cart.length || d.submitting) return
    this.setData({ submitting: true })
    try {
      const items: CreateOrderItem[] = d.cart.map((l) => ({
        product_id: l.product_id,
        qty: l.qty,
      }))
      const order = await createPendingOrder({
        channel: 'in_store',
        payment_method: 'cash',
        items,
      })
      const totalYuan = yuan(order.TotalCents)
      const ok = await confirmPay(totalYuan)
      if (!ok) {
        // 用户暂不收款 → 购物车保留, 订单已是 pending, 可在订单管理里后续处理
        wx.showToast({ title: '已挂单', icon: 'none' })
        return
      }
      const paid = await payOrder(order.ID)
      this.setData({
        receipt: buildReceipt(paid),
        cart: [],
        totalCount: 0,
        totalYuan: '0',
      })
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : '开单失败'
      wx.showToast({ title: msg, icon: 'none' })
    } finally {
      this.setData({ submitting: false })
    }
  },

  // === 回执「再开一单」 ===
  onNewOrder() {
    this.setData({ receipt: null })
  },

  // === 内部: 加商品到购物车 (同 product 累加) ===
  addToCart(p: Product, delta: number) {
    const d = this.data as BillingData
    const cart = d.cart.slice()
    const idx = cart.findIndex((l) => l.product_id === p.ID)
    if (idx >= 0) {
      const line = cart[idx]
      const qty = line.qty + delta
      cart[idx] = { ...line, qty, subtotal_cents: qty * line.price_cents, subtotal_yuan: yuan(qty * line.price_cents) }
    } else {
      cart.push({
        product_id: p.ID,
        name: p.Name,
        unit: p.Unit || '',
        price_cents: p.PriceCents,
        price_yuan: yuan(p.PriceCents),
        qty: delta,
        subtotal_cents: delta * p.PriceCents,
        subtotal_yuan: yuan(delta * p.PriceCents),
      })
    }
    this.applyCart(cart)
  },

  // === 内部: 改数量 (qty<=0 移除) ===
  setCartQty(productId: number, qty: number) {
    const d = this.data as BillingData
    let cart = d.cart.slice()
    if (qty <= 0) {
      cart = cart.filter((l) => l.product_id !== productId)
    } else {
      const idx = cart.findIndex((l) => l.product_id === productId)
      if (idx < 0) return
      const line = cart[idx]
      cart[idx] = {
        ...line,
        qty,
        subtotal_cents: qty * line.price_cents,
        subtotal_yuan: yuan(qty * line.price_cents),
      }
    }
    this.applyCart(cart)
  },

  findQty(productId: number): number {
    const d = this.data as BillingData
    const line = (d.cart || []).find((l) => l.product_id === productId)
    return line ? line.qty : 0
  },

  // === 内部: 应用购物车 + 重算合计 ===
  applyCart(cart: CartLine[]) {
    let count = 0
    let totalCents = 0
    for (const l of cart) {
      count += l.qty
      totalCents += l.subtotal_cents
    }
    this.setData({
      cart,
      totalCount: count,
      totalYuan: yuan(totalCents),
    })
  },
})

/** 后端金额 int64 分 → 元(整除显示整数,否则两位小数) */
function yuan(cents: number | string): string {
  const v = Number(cents) / 100
  return v % 1 === 0 ? String(v) : v.toFixed(2)
}

/** 「确认收款」弹窗 (Promise 化) */
function confirmPay(totalYuan: string): Promise<boolean> {
  return new Promise((resolve) => {
    wx.showModal({
      title: '确认收款',
      content: `实收 ¥${totalYuan}`,
      confirmText: '已收款',
      confirmColor: '#FF6B35',
      cancelText: '挂单',
      success: (r) => resolve(!!r.confirm),
      fail: () => resolve(false),
    })
  })
}

/** 构造回执视图模型 */
function buildReceipt(o: Order): ReceiptVM {
  const count = (o.Items || []).reduce((acc, it) => acc + (it.Qty || 0), 0)
  return {
    order_no: o.OrderNo || String(o.ID),
    total_yuan: yuan(o.TotalCents),
    count,
    paid_at: formatTime(o.PaidAt || o.CreatedAt),
  }
}

/** ISO 时间 → 本地 HH:mm:ss (兜底 YYYY-MM-DD HH:mm) */
function formatTime(iso: string | undefined): string {
  if (!iso) return ''
  const t = new Date(iso)
  if (isNaN(t.getTime())) return iso
  const pad = (n: number) => (n < 10 ? '0' + n : '' + n)
  return `${t.getFullYear()}-${pad(t.getMonth() + 1)}-${pad(t.getDate())} ${pad(t.getHours())}:${pad(t.getMinutes())}:${pad(t.getSeconds())}`
}
