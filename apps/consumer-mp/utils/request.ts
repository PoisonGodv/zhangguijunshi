// utils/request.ts — C 端 JWT 鉴权请求封装 (技术方案 §9.2.5 + 后端 /v1/wxshop/auth 端点)
//
// 职责: baseURL 注入 + Bearer 鉴权 + envelope 解包 (code===0 取 data) +
//       401→refresh→重发一次 (防死循环) + errcode 中文映射 + 幂等 GET 网络重试。
//
// 与 merchant-mp/utils/request.ts 的差异: 那是 B 端 X-Mock-Tenant 直连;
// 这里走真实 JWT (login → access/refresh → me),鉴权失效时静默 refresh,refresh 也失效
// 则清空 token 强制下次 wxLogin。

import type { Envelope, WxLoginResp } from '../types/api'

// === 全局 globalData 形状 (与 app.ts 一致; 工程地基 agent 负责 app.ts) ===
interface AppGlobalData {
  baseURL: string
  tenantID: number
}

export function appGlobal(): AppGlobalData {
  // getApp() 在 onLaunch 后可用;防御性兜底
  const app = getApp() as unknown as { globalData?: AppGlobalData } | undefined
  if (app && app.globalData && app.globalData.baseURL) return app.globalData
  return { baseURL: 'http://localhost:8080', tenantID: 1 }
}

// === token 存储 (key 与 utils/auth.ts 保持一致) ===
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

// === errcode → 中文映射 (C 端契约; 业务码 HTTP 200 + code,鉴权码 401 + code) ===
// 10001 未登录 / 10002 登录已过期 命中鉴权链路,不在此映射 (走 refresh)。
// 6xxx 与后端 pkg/errcode 对齐 (ErrGroupbuyFull=60013 等); 未覆盖码回落 body.message。
const ERR_CODE_MSG: Record<number, string> = {
  10003: '无权限',
  30001: '会员码已过期',
  30002: '储值余额不足',
  30003: '拼团已满员',
  30004: '触达太频繁请稍后',
  42302: '会员已被冻结',
  60005: '券已发完',
  60006: '券未上线或不在有效期',
  60007: '券已过期',
  60008: '券已核销',
  60013: '拼团已满员',
  60015: '您已参团,请勿重复',
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

// === 并发去重: 同时多个请求触发 login/refresh 时复用同一个 promise ===
let loggingIn: Promise<string> | null = null
let refreshing: Promise<string> | null = null

/**
 * wxLogin: wx.login → code → POST /v1/wxshop/auth/login {code, tenant_id} → 存 token。
 * 返回原始 WxLoginResp;storage 写入在此完成。
 */
export function wxLogin(): Promise<WxLoginResp> {
  return new Promise<WxLoginResp>((resolve, reject) => {
    wx.login({
      success: (res) => {
        if (!res.code) {
          reject(new ApiError(-1, 'wx.login 未返回 code'))
          return
        }
        wx.request({
          url: appGlobal().baseURL + '/v1/wxshop/auth/login',
          method: 'POST',
          timeout: 10000,
          header: { 'Content-Type': 'application/json', 'X-Client': 'consumer-mp' },
          data: { code: res.code, tenant_id: appGlobal().tenantID },
          success: (r) => {
            const body = (r.data as Envelope<WxLoginResp>) || ({ code: -1 } as Envelope<WxLoginResp>)
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
      },
      fail: (err) => reject(new ApiError(-1, err.errMsg || 'wx.login 失败')),
    })
  })
}

/**
 * ensureToken: 有 access 直接返回;否则 wxLogin。并发去重复用 loggingIn promise。
 */
export function ensureToken(): Promise<string> {
  const t = getAccessToken()
  if (t) return Promise.resolve(t)
  if (loggingIn) return loggingIn
  loggingIn = (async () => {
    const res = await wxLogin()
    return res.access_token
  })()
  return loggingIn.then(
    (token) => { loggingIn = null; return token },
    (err) => { loggingIn = null; throw err },
  )
}

/**
 * refreshToken: 用 refresh_token 调 POST /v1/wxshop/auth/refresh (X-Refresh-Token 头),
 * 成功更新 storage 并返回新 access;失败清空两个 token (强制下次重新 login)。并发去重。
 */
export function refreshToken(): Promise<string> {
  if (refreshing) return refreshing
  refreshing = (async () => {
    const refresh = getRefreshToken()
    if (!refresh) throw new ApiError(10002, '登录已过期')
    const resp = await new Promise<WxLoginResp>((resolve, reject) => {
      wx.request({
        url: appGlobal().baseURL + '/v1/wxshop/auth/refresh',
        method: 'POST',
        timeout: 10000,
        header: {
          'Content-Type': 'application/json',
          'X-Client': 'consumer-mp',
          'X-Refresh-Token': refresh,
        },
        success: (r) => {
          const body = (r.data as Envelope<WxLoginResp>) || ({ code: -1 } as Envelope<WxLoginResp>)
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
    (err) => { refreshing = null; clearTokens(); throw err }, // refresh 失败清空,强制重新 login
  )
}

interface RawOpts {
  url: string
  method: Method
  data?: unknown
  retry?: number   // 仅 GET 网络失败时递减重试
  silent?: boolean // 预留: true 时不弹 toast (当前由页面自行捕获 ApiError)
}

/**
 * rawRequest<T>: 统一 wx.request Promise 封装。
 *  - 注入 Authorization/X-Client/X-Trace-Id/Content-Type
 *  - 解 envelope: code===0 resolve(data)
 *  - statusCode 401 或 body.code 为 10001/10002 → 清 access → refreshToken → 只重发一次 (retried 闭包标志防死循环)
 *  - 其余 code → errcode 中文映射 reject(ApiError)
 *  - 网络 fail 且 GET 且 retry>0 → 800ms 递减重试
 */
export function rawRequest<T>(opts: RawOpts): Promise<T> {
  // 防死循环: 本次调用链内是否已因 401 重发过一次
  let retried = false

  const attempt = async (retryLeft: number): Promise<T> => {
    const access = await ensureToken()
    const traceId = `${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
    return new Promise<T>((resolve, reject) => {
      wx.request({
        url: appGlobal().baseURL + opts.url,
        method: opts.method,
        data: opts.data as Record<string, unknown> | string | undefined,
        timeout: 10000,
        header: {
          'Authorization': `Bearer ${access}`,
          'X-Client': 'consumer-mp',
          'X-Trace-Id': traceId,
          'Content-Type': 'application/json',
        },
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
          if (opts.method === 'GET' && retryLeft > 0) {
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

  return attempt(opts.retry ?? 0)
}

// === http 便捷方法: get 默认重试 2 次,写操作不重试 ===
export const http = {
  get: <T>(url: string, data?: unknown, silent?: boolean): Promise<T> =>
    rawRequest<T>({ url, method: 'GET', data, retry: 2, silent }),
  post: <T>(url: string, data?: unknown, silent?: boolean): Promise<T> =>
    rawRequest<T>({ url, method: 'POST', data, silent }),
  put: <T>(url: string, data?: unknown, silent?: boolean): Promise<T> =>
    rawRequest<T>({ url, method: 'PUT', data, silent }),
  del: <T>(url: string, silent?: boolean): Promise<T> =>
    rawRequest<T>({ url, method: 'DELETE', silent }),
}

/** logoutReq: POST /v1/wxshop/auth/logout (带 Bearer),无论成败都清空本地 token。 */
export async function logoutReq(): Promise<void> {
  try {
    await rawRequest<unknown>({ url: '/v1/wxshop/auth/logout', method: 'POST' })
  } catch (_e) {
    // 登出失败不阻断本地清理 (token 可能已失效)
  } finally {
    clearTokens()
  }
}
