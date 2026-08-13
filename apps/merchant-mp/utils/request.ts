// utils/request.ts — B 端 JWT 鉴权请求封装 (技术方案 §9.2.5 + CONTRACT §19 鉴权端点)
//
// 职责: baseURL 注入 + 鉴权头 (Bearer 优先,X-Mock-Tenant dev 兜底) + envelope 解包 (code===0 取 data) +
//       401/10001/10002→refresh→只重发一次 (防死循环) + errcode 中文映射 + 幂等 GET 网络重试。
//
// 鉴权范式照抄 consumer-mp/utils/request.ts:token 存 storage (zgjs_access/zgjs_refresh)、
// ensureToken 并发去重、refresh 失败清双 token。差异: B 端登录是 boss 员工账密登录
// (POST /v1/boss/auth/login),无 wx.login;无 token 时保留 X-Mock-Tenant dev 兜底
// (globalData.tenantHeader,BE-A-01 真登录后 dev 仍可用 mock 头直连)。
//
// ARCH-01: tenant/store/role 永远由后端从 JWT ctx 解析,前端绝不信任 body/localStorage 里的租户信息。

import type {
  Envelope,
  LoginResp,
  Advice,
  Strategy,
  ExecResult,
  ExecuteBody,
  DailySummary,
  SubscriptionResp,
  Whoami,
  ListMembersResp,
  MemberDTO,
  RechargeResult,
  Product,
  ProductListResp,
  Order,
  CreateOrderReq,
  RedeemReq,
  RedeemResult,
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

// === token 存储 (key 与 consumer-mp 一致: zgjs_access/zgjs_refresh) ===
const KEY_ACCESS = 'zgjs_access'
const KEY_REFRESH = 'zgjs_refresh'

export function getAccessToken(): string {
  return wx.getStorageSync(KEY_ACCESS) || ''
}
export function getRefreshToken(): string {
  return wx.getStorageSync(KEY_REFRESH) || ''
}
function saveTokens(access: string, refresh: string): void {
  wx.setStorageSync(KEY_ACCESS, access)
  if (refresh) wx.setStorageSync(KEY_REFRESH, refresh)
}
export function clearTokens(): void {
  wx.removeStorageSync(KEY_ACCESS)
  wx.removeStorageSync(KEY_REFRESH)
}

// === errcode → 中文映射 (CONTRACT §12.7) ===
// 10001 未登录 / 10002 登录已过期 命中鉴权链路,不在此映射 (走 refresh)。
// 降级非失败 (60018-60020 / Advice.degrade) 不走这里,由 today 页 degrade 提示条展示。
const ERR_CODE_MSG: Record<number, string> = {
  40404: '策略不存在',
  10003: '无权限',
  60017: '毛利护栏拦截',
  60018: 'AI 预算熔断,已降级',
  60019: 'AI 增强异常,已降级',
  60020: '数据不足,已降级',
  60021: '动作被拦截',
  60022: '该动作暂不支持',
  60003: '储值余额超过单店上限',
  60004: '储值余额不足',
  40003: '会员码无效或已过期',
  42302: '会员已被冻结',
  40402: '会员不存在',
}
const FALLBACK_SYSTEM_MSG = '系统异常请重试'

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

// === 并发去重: 同时多个请求触发 refresh 时复用同一个 promise ===
let refreshing: Promise<string> | null = null

/**
 * bossLogin: 商家员工账密登录。
 * POST /v1/boss/auth/login {tenant_id, phone, password} → {access_token, refresh_token, expires_in?}
 * 存 token 到 storage 并返回 LoginResp;失败统一 ErrLoginFailed (后端不区分账号不存在/密码错)。
 */
export function bossLogin(tenantID: number, phone: string, password: string): Promise<LoginResp> {
  return new Promise<LoginResp>((resolve, reject) => {
    wx.request({
      url: appGlobal().baseURL + '/v1/boss/auth/login',
      method: 'POST',
      timeout: 10000,
      header: { 'Content-Type': 'application/json', 'X-Client': 'merchant-mp' },
      data: { tenant_id: tenantID, phone, password },
      success: (r) => {
        const body = (r.data as Envelope<LoginResp>) || ({ code: -1 } as Envelope<LoginResp>)
        if (r.statusCode === 200 && body.code === 0 && body.data && body.data.access_token) {
          saveTokens(body.data.access_token, body.data.refresh_token)
          resolve(body.data)
          return
        }
        clearTokens()
        const code = typeof body.code === 'number' ? body.code : -1
        reject(new ApiError(code, body.message || '登录失败', body.trace_id))
      },
      fail: (err) => reject(new ApiError(-1, err.errMsg || '网络异常')),
    })
  })
}

/**
 * ensureToken: 有 access 直接返回;无 access 但有 refresh → 静默续期;两者皆无 → reject 10002。
 * (B 端登录需要手机号+密码,无法自动重登,由页面捕获 10002 后跳登录页。)
 */
export function ensureToken(): Promise<string> {
  const t = getAccessToken()
  if (t) return Promise.resolve(t)
  if (getRefreshToken()) return refreshToken()
  return Promise.reject(new ApiError(10002, '登录已过期'))
}

/** isLoggedIn: 本地存在 access token (不校验有效期,过期由请求层 refresh 兜底) */
export function isLoggedIn(): boolean {
  return !!getAccessToken()
}

/**
 * refreshToken: 用 refresh_token 调 POST /v1/wxshop/auth/refresh (X-Refresh-Token 头),
 * 成功更新 storage 并返回新 access;失败清空两个 token (页面捕获后跳登录页)。并发去重。
 */
export function refreshToken(): Promise<string> {
  if (refreshing) return refreshing
  refreshing = (async () => {
    const refresh = getRefreshToken()
    if (!refresh) throw new ApiError(10002, '登录已过期')
    const resp = await new Promise<LoginResp>((resolve, reject) => {
      wx.request({
        url: appGlobal().baseURL + '/v1/wxshop/auth/refresh',
        method: 'POST',
        timeout: 10000,
        header: {
          'Content-Type': 'application/json',
          'X-Client': 'merchant-mp',
          'X-Refresh-Token': refresh,
        },
        success: (r) => {
          const body = (r.data as Envelope<LoginResp>) || ({ code: -1 } as Envelope<LoginResp>)
          if (r.statusCode === 200 && body.code === 0 && body.data && body.data.access_token) {
            resolve(body.data)
            return
          }
          const code = typeof body.code === 'number' ? body.code : -1
          reject(new ApiError(code, body.message || '续期失败', body.trace_id))
        },
        fail: (err) => reject(new ApiError(-1, err.errMsg || '网络异常')),
      })
    })
    saveTokens(resp.access_token, resp.refresh_token)
    return resp.access_token
  })()
  return refreshing.then(
    (token) => { refreshing = null; return token },
    (err) => { refreshing = null; clearTokens(); throw err }, // refresh 失败清空,强制重新登录
  )
}

/**
 * attachAuth: 鉴权头钩子 —— 有 access token → Authorization: Bearer (prod 真登录);
 * 无 token → 保留 X-Mock-Tenant dev 兜底 (globalData.tenantHeader,与 admin-web「真登录优先、dev 回落」一致)。
 */
export function attachAuth(header: Record<string, string>): Record<string, string> {
  const token = getAccessToken()
  if (token) return { ...header, 'Authorization': `Bearer ${token}` }
  const { tenantHeader } = appGlobal()
  return { ...tenantHeader, ...header }
}

interface RawOpts {
  url: string
  method: Method
  data?: unknown
  retry?: number   // 仅 GET 网络失败时递减重试
}

/**
 * request<T>: 统一 wx.request Promise 封装。
 *  - header: attachAuth 注入 Bearer/X-Mock-Tenant + Content-Type: application/json
 *  - 解 envelope: code===0 resolve(data)
 *  - statusCode 401 或 body.code 为 10001/10002 → 清 access → refreshToken → 只重发一次 (retried 闭包防死循环)
 *  - 其余 code → errcode 中文映射 reject(ApiError)
 *  - 网络 fail 且 GET 且 retry>0 → 800ms 递减重试
 *  - 最终鉴权失败: 清 token 并抛 10002 (页面捕获后跳登录页)
 */
export function request<T>(method: Method, path: string, data?: unknown): Promise<T> {
  // 防死循环: 本次调用链内是否已因 401 重发过一次
  let retried = false

  const attempt = async (retryLeft: number): Promise<T> => {
    const traceId = `${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
    return new Promise<T>((resolve, reject) => {
      wx.request({
        url: appGlobal().baseURL + path,
        method,
        data: data as Record<string, unknown> | string | undefined,
        timeout: 10000,
        header: attachAuth({ 'Content-Type': 'application/json', 'X-Client': 'merchant-mp', 'X-Trace-Id': traceId }),
        success: (res) => {
          const body = ((res.data as Envelope<T>) || { code: -1, message: '' }) as Envelope<T>
          const code = (body && typeof body.code === 'number') ? body.code : (res.statusCode === 200 ? 0 : -1)

          // 1) 成功
          if (res.statusCode === 200 && code === 0) {
            resolve(body.data)
            return
          }

          // 2) 鉴权失效: 清 access → refresh → 只重发一次
          const isAuthFail = res.statusCode === 401 || code === 10001 || code === 10002
          if (isAuthFail) {
            if (retried) {
              // 已重发过一次仍失败: refresh 也救不回,清空强制重新登录
              clearTokens()
              reject(new ApiError(10002, '登录已过期', body && body.trace_id))
              return
            }
            retried = true
            wx.removeStorageSync(KEY_ACCESS) // 清 access 保留 refresh
            refreshToken()
              .then(() => attempt(retryLeft).then(resolve, reject))
              .catch((e) => reject(e instanceof ApiError ? e : new ApiError(10002, '登录已过期')))
            return
          }

          // 3) 业务/系统错误: 中文映射
          let msg = ERR_CODE_MSG[code]
          if (!msg) {
            if (code >= 50000) msg = FALLBACK_SYSTEM_MSG
            else msg = (body && body.message) || '请求失败'
          }
          reject(new ApiError(code, msg, body && body.trace_id))
        },
        fail: (err) => {
          // 4) 网络层失败: 仅幂等 GET 重试
          if (method === 'GET' && retryLeft > 0) {
            setTimeout(() => {
              attempt(retryLeft - 1).then(resolve, reject)
            }, 800)
            return
          }
          reject(new ApiError(-1, err.errMsg || '网络异常', traceId))
        },
      })
    })
  }

  return attempt(method === 'GET' ? 2 : 0)
}

/** logoutReq: POST /v1/boss/auth/logout (带 Bearer),无论成败都清空本地 token。 */
export async function logoutReq(): Promise<void> {
  try {
    await request<unknown>('POST', '/v1/boss/auth/logout')
  } catch (_e) {
    // 登出失败不阻断本地清理 (token 可能已失效)
  } finally {
    clearTokens()
  }
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
 * storeID 由后端从 JWT/MockTenant ctx 解析 (ARCH-01),无需前端传。
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

/**
 * getMemberCount: 会员总数 (me 页会员数卡片, CONTRACT §24.7 路线 A,后端 0 改动)。
 * 复用已有 GET /v1/boss/membership/members (§17.11 ListMembersPage 已返 {items,total}),
 * page=1&size=1 最小拉取,只取 total;total 缺失/非法时兜底 0。
 */
export async function getMemberCount(): Promise<number> {
  const resp = await request<ListMembersResp>('GET', '/v1/boss/membership/members?page=1&size=1')
  const total = resp && typeof resp.total === 'number' ? resp.total : 0
  return total
}

// === 充值页 (MVP-3, CONTRACT §25.3) —— 会员搜索 + 手动充值 ===

/**
 * searchMembers: 会员关键词搜索 (充值页选目标会员)。
 * GET /v1/boss/membership/members?q={关键词}&page=1&size=20 → {items: memberDTO[], total}
 * q 模糊匹配 Name/PhoneHash (后端 MemberFilter.Query);返回 memberDTO (snake_case)。
 */
export function searchMembers(query: string, page = 1, size = 20): Promise<ListMembersResp> {
  const qs: string[] = [`page=${page}`, `size=${size}`]
  if (query) qs.push(`q=${encodeURIComponent(query)}`)
  return request<ListMembersResp>('GET', `/v1/boss/membership/members?${qs.join('&')}`)
}

/**
 * rechargeMember: 店员手动充值 (MVP-3, 不做真实支付, 直接入账)。
 * POST /v1/boss/membership/members/:id/recharge  body {amount_cents:int} → RechargeResult
 * 赠送由后端 GiftForAmount 档位规则自动算,前端绝不传 gift。失败 errcode:
 * 60003 超单店上限 / 42302 会员冻结 / 40402 会员不存在 / 40001 参数错。
 */
export function rechargeMember(memberID: number, amountCents: number): Promise<RechargeResult> {
  return request<RechargeResult>('POST', `/v1/boss/membership/members/${memberID}/recharge`, {
    amount_cents: amountCents,
  })
}

/**
 * redeemMemberCode: 会员码核销 (顾客出示动态码,店员核销扣储值/积积分)。
 * POST /v1/boss/membership/redeem  body {code, consume_cents?, earn_points?} → RedeemResult
 * code 必填;consume_cents/earn_points 至少一个 >0。失败 errcode:
 * 40003 会员码无效或已过期 / 60004 储值余额不足 / 42302 会员冻结 / 40001 参数错。
 * 注: 核销只记储值+积分流水,不开销售单 (见 types/api.d.ts RedeemResult 注释)。
 */
export function redeemMemberCode(body: RedeemReq): Promise<RedeemResult> {
  return request<RedeemResult>('POST', '/v1/boss/membership/redeem', body)
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
