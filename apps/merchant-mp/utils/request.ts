// utils/request.ts — wx.request 封装 (技术方案 §9.2.5 范式 + CONTRACT §12.7 端点/errcode)
//
// 职责: baseURL 注入 + 鉴权头 (attachAuth 钩子) + envelope 解包 (code===0 取 data) +
//       errcode 中文映射 + 三种 AI 引擎 API (getToday/getStrategy/executeItem)。

import type {
  Envelope,
  Advice,
  Strategy,
  ExecResult,
  ExecuteBody,
  DailySummary,
  SubscriptionResp,
  Whoami,
  Product,
  ProductListResp,
  Order,
  CreateOrderReq,
} from '../types/api'

// === 全局 globalData 形状 (与 app.ts 一致) ===
interface AppGlobalData {
  baseURL: string
  tenantHeader: Record<string, string>
}

function appGlobal(): AppGlobalData {
  // getApp() 在页面 onLaunch 后可用;防御性兜底
  const app = getApp() as unknown as { globalData?: AppGlobalData } | undefined
  if (app && app.globalData) return app.globalData
  return { baseURL: 'http://localhost:8080', tenantHeader: {} }
}

// === 鉴权头钩子 (FE-MP-02 接 JWT 时只改这处) ===
// 当前: 注入 app.globalData.tenantHeader (X-Mock-Tenant: 1) + Content-Type。
// M0-S2 后: 在此读取本地 access token,追加 'Authorization': `Bearer ${token}`。
export function attachAuth(header: Record<string, string>): Record<string, string> {
  const { tenantHeader } = appGlobal()
  return { ...tenantHeader, ...header }
}

// === errcode → 中文映射 (CONTRACT §12.7) ===
// 降级非失败 (60018-60020 / Advice.degrade) 不走这里,由 today 页 degrade 提示条展示。
const ERR_CODE_MSG: Record<number, string> = {
  40404: '策略不存在',
  60017: '毛利护栏拦截',
  60018: 'AI 预算熔断,已降级',
  60019: 'AI 增强异常,已降级',
  60020: '数据不足,已降级',
  60021: '动作被拦截',
  60022: '该动作暂不支持',
}

/** ApiError: 携带后端 errcode + trace_id 的错误对象 */
export class ApiError extends Error {
  code: number
  trace_id?: string
  constructor(code: number, message: string, trace_id?: string) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.trace_id = trace_id
  }
}

type Method = 'GET' | 'POST' | 'PUT' | 'DELETE'

/**
 * request<T>: 统一 wx.request Promise 封装。
 *  - header: attachAuth 注入租户头 + Content-Type: application/json
 *  - 解 envelope: code===0 resolve(data);否则按 ERR_CODE_MSG 映射中文 reject(ApiError)
 */
export function request<T>(method: Method, path: string, data?: unknown): Promise<T> {
  const baseURL = appGlobal().baseURL
  return new Promise<T>((resolve, reject) => {
    wx.request({
      url: baseURL + path,
      method,
      data: data as Record<string, unknown> | string | undefined,
      timeout: 10000,
      header: attachAuth({ 'Content-Type': 'application/json' }),
      success: (res) => {
        const body = ((res.data as Envelope<T>) || { code: -1, message: '' }) as Envelope<T>
        if (body && body.code === 0) {
          resolve(body.data)
          return
        }
        const code = (body && typeof body.code === 'number') ? body.code : -1
        const msg = ERR_CODE_MSG[code] || (body && body.message) || '请求失败'
        reject(new ApiError(code, msg, body && body.trace_id))
      },
      fail: (err) => {
        reject(new ApiError(-1, err.errMsg || '网络异常'))
      },
    })
  })
}

// === AI 引擎 API (CONTRACT §12.7 端点) ===

/**
 * getToday: 拉取「今日3件事」建议。
 * 契约 §12.7: POST /v1/boss/aiengine/today  body {date?} → *output.Advice (200,降级也 200 带 degrade)
 * 注: 任务文 #3 写作 GET,但数据契约权威 §12.7 明确为 POST,以契约为准。
 */
export function getToday(date?: string): Promise<Advice> {
  return request<Advice>('POST', '/v1/boss/aiengine/today', date ? { date } : {})
}

/** getStrategy: 拉取单条策略详情 (含 items)。契约 §12.7: GET /v1/boss/aiengine/strategies/:id */
export function getStrategy(id: number): Promise<Strategy> {
  return request<Strategy>('GET', `/v1/boss/aiengine/strategies/${id}`)
}

/**
 * executeItem: 一键执行单条建议。
 * 契约 §12.7: POST /v1/boss/aiengine/strategies/:id/items/:itemId/execute  body {confirm?} → *exec.ExecResult (blocked 也 200)
 * 统一传 confirm:true (price_adj 必需,其他 action_type 忽略)。
 */
export function executeItem(id: number, itemId: number, body: ExecuteBody = {}): Promise<ExecResult> {
  return request<ExecResult>(
    'POST',
    `/v1/boss/aiengine/strategies/${id}/items/${itemId}/execute`,
    { confirm: true, ...body },
  )
}

// === me 我的页 (FE-MP-04) —— 经营/订阅/门店 BFF ===

/**
 * getDailySummaryToday: 当日流水摘要。
 * GET /v1/boss/billing/summary/today → billing.DailySummary (无 json tag,字段 PascalCase)。
 * storeID 由后端从 ctx (X-Mock-Tenant 注入的 MockTenant) 解析,无需前端传。
 */
export function getDailySummaryToday(): Promise<DailySummary> {
  return request<DailySummary>('GET', '/v1/boss/billing/summary/today')
}

/**
 * getSubscription: 当前订阅状态。
 * GET /v1/boss/subscription → { plan, subscription } (handler GetSubscription,gin.H 键小写)
 * subscription 可能为 null (无订阅记录,默认 free)。
 */
export function getSubscription(): Promise<SubscriptionResp> {
  return request<SubscriptionResp>('GET', '/v1/boss/subscription')
}

/**
 * getWhoami: 当前租户身份 (用于 me 页门店/身份卡)。
 * GET /v1/boss/whoami → { tenant_id, source } (router.go whoami,gin.H 键 snake_case)。
 * 后端目前不返回 store_id/role/scene,前端只用 tenant_id + source。
 */
export function getWhoami(): Promise<Whoami> {
  return request<Whoami>('GET', '/v1/boss/whoami')
}

// === billing 开单页 (FE-MP-02) —— 商品/订单 API ===

/**
 * listProducts: 商品分页列表。
 * GET /v1/boss/catalog/products?limit=&offset=&query= → {items, count}
 * 后端无 page/page_size, 用 limit/offset;keyword 走 query 模糊匹配名称/条码。
 */
export function listProducts(limit: number, offset: number, keyword?: string): Promise<ProductListResp> {
  const qs: string[] = [`limit=${limit}`, `offset=${offset}`]
  if (keyword) qs.push(`query=${encodeURIComponent(keyword)}`)
  return request<ProductListResp>('GET', `/v1/boss/catalog/products?${qs.join('&')}`)
}

/**
 * getProductByBarcode: 按条码查商品 (扫码开单)。
 * GET /v1/boss/catalog/products/barcode/:barcode → Product
 * 未命中 → 40401 ApiError。
 */
export function getProductByBarcode(barcode: string): Promise<Product> {
  return request<Product>('GET', `/v1/boss/catalog/products/barcode/${encodeURIComponent(barcode)}`)
}

/**
 * createOrder: 直接创建已收款订单 (M1-S1 一步到位,status=paid,自动 FIFO 扣库存)。
 * POST /v1/boss/billing/orders  body CreateOrderReq → Order
 */
export function createOrder(body: CreateOrderReq): Promise<Order> {
  return request<Order>('POST', '/v1/boss/billing/orders', body)
}

/**
 * createPendingOrder: 创建挂单 (M4-S1,status=pending,不扣库存)。
 * POST /v1/boss/billing/orders/pending  body CreateOrderReq → Order
 * 配合 payOrder 走「开单→确认收款→回执」两段式 UX。
 */
export function createPendingOrder(body: CreateOrderReq): Promise<Order> {
  return request<Order>('POST', '/v1/boss/billing/orders/pending', body)
}

/**
 * payOrder: 模拟收款 (pending→paid, M4-S1 SimulatePay)。
 * POST /v1/boss/billing/orders/:id/pay → Order
 * 已 paid → 40903 ErrStatusConflict;跨租户/不存在 → 40401。
 */
export function payOrder(id: number): Promise<Order> {
  return request<Order>('POST', `/v1/boss/billing/orders/${id}/pay`)
}
