// src/services/request.ts — 管理后台 axios 请求封装 (FE-ADMIN-01)
//
// 范式对齐 apps/consumer-mp/utils/request.ts 与 apps/merchant-mp/utils/request.ts:
//   - baseURL '/v1' (vite proxy 转发到后端 :8080)
//   - 请求拦截: 注入 dev 鉴权头 (X-Mock-Tenant/Store/User/Role) + X-Client + X-Trace-Id
//   - 响应拦截: 解 envelope (code===0 返 data; 否则 message 提示 + reject); 401 清理
//   - errcode 中文映射 (复用 consumer-mp 思路, 补 boss 域码)
//
// 鉴权模式:
//   - 当前 dev: X-Mock-* 请求头直连 (与 apps/merchant-mp 一致, BE-A-01 真实登录延后)。
//   - prod 预留: Authorization 注入位 (见下方 PROD 注释, BE-A-01 落地后改)。

import axios, { type AxiosInstance, type AxiosRequestConfig, type AxiosResponse, type InternalAxiosRequestConfig } from 'axios'
import { message } from 'antd'
import type { Envelope } from '@/types/api'

// === dev 兜底鉴权头 (BE-A-01 已落地: 真登录优先, 仅 dev 未登录时兜底) ===
// 鉴权顺序 (见请求拦截器): localStorage.admin_token → Authorization: Bearer <jwt>;
// 无 token 且处于 dev (import.meta.env.DEV) 时, 回落 X-Mock-* 头直连, 便于后端
// 未开真登录/本地无账号时联调。prod 构建 (import.meta.env.DEV=false) 永不注入 mock 头。
const DEV_HEADERS: Record<string, string> = {
  'X-Mock-Tenant': '1',
  'X-Mock-Store': '0',
  'X-Mock-User': 'dev',
  'X-Mock-Role': 'owner',
}

// === 登录态存取 (BE-A-01) ===
const TOKEN_KEY = 'admin_token'
export const getToken = (): string | null => localStorage.getItem(TOKEN_KEY)
export const setToken = (token: string): void => localStorage.setItem(TOKEN_KEY, token)
export const clearToken = (): void => localStorage.removeItem(TOKEN_KEY)

// 跳登录页 (避免在拦截器里 import router 造成循环; 直接改 location)。
function redirectToLogin(): void {
  if (typeof window !== 'undefined' && window.location.pathname !== '/login') {
    window.location.href = '/login'
  }
}

// === errcode → 中文映射 (boss 域, 对齐后端 pkg/errcode + handler 内联码) ===
// 格式: HTTP×100 + 业务子码; 业务拒绝用 HTTP 200 + 6xxx, 前端按 code 分支。
// 未覆盖码回落 body.message; code>=50000 视为系统错误兜底。
const ERR_CODE_MSG: Record<number, string> = {
  // 鉴权 (401 类, dev 不触发, prod BE-A-01 落地后用)
  40101: '登录已失效，请重新登录',
  40103: '支付回调验签失败',
  10001: '未登录',           // 兼容旧码
  10002: '登录已过期',
  10003: '无权限',
  // 参数 / 资源 (400/404 类)
  40001: '参数错误',
  40002: '库存不足',
  40003: '会员码无效或已过期',
  40301: '无权操作该资源',
  40302: '跨租户访问被拒',
  40303: '当前套餐不支持该功能，请升级',
  40401: '资源不存在',
  40402: '会员不存在',
  40403: '券不存在',
  40404: '策略不存在',        // 兼容拼团/触达/社群/活码/策略
  40901: '重复请求已处理',
  40902: '支付单号重复',
  40903: '订单状态不可推进',
  // 限流 / 锁定
  42301: '账号已锁定，15 分钟后重试',
  42302: '会员已被冻结',
  42901: '操作过于频繁，请稍后再试',
  // 业务拒绝 (HTTP 200 + 6xxx)
  60001: '折扣低于毛利护栏，已拦截',
  60002: '群发频次已达上限',
  60003: '储值余额超过单店上限',
  60004: '储值余额不足',
  60005: '券已发完',
  60006: '券未上线或不在有效期',
  60007: '券已过期',
  60008: '券已核销',
  60009: '内容命中敏感词，已拦截',
  60010: '订阅消息授权次数已用尽',
  60011: '会员已退订该通道',
  60012: '触达模板不存在',
  60013: '拼团已满',
  60014: '拼团已结束或不可参团',
  60015: '该会员已参团',
  60016: '社群SOP已到终态',
  60017: '执行时毛利护栏拦截',
  60018: 'AI预算熔断，已降级为规则建议',
  60019: 'AI增强失败，已降级为规则建议',
  60020: '数据不足，已使用冷启动剧本',
  60021: '动作被合规拦截，未执行',
  60022: '该动作暂不可一键执行',
  60023: '特约商户未进件或未激活',
}
const FALLBACK_SYSTEM_MSG = '系统异常请重试'

/** ApiError: 携带后端 errcode + trace_id 的错误对象 */
export class ApiError extends Error {
  code: number
  trace_id?: string
  constructor(code: number, msg: string, trace_id?: string) {
    super(msg)
    this.name = 'ApiError'
    this.code = code
    this.trace_id = trace_id
  }
}

// === trace id 生成 (浏览器无 crypto.randomUUID 兜底) ===
function genTraceId(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  return `${Date.now()}-${Math.random().toString(36).slice(2, 10)}`
}

// === axios 实例 ===
const client: AxiosInstance = axios.create({
  baseURL: '/v1',
  timeout: 15000,
  headers: { 'Content-Type': 'application/json' },
})

// 请求拦截: 真登录 token 优先; dev 未登录回落 X-Mock-* 头; 注入 trace id + client。
client.interceptors.request.use((cfg: InternalAxiosRequestConfig) => {
  const token = getToken()
  if (token) {
    // BE-A-01 真登录: 所有业务请求 tenant 来自 JWT, 绝不带 mock 头。
    cfg.headers.set('Authorization', `Bearer ${token}`)
  } else if (import.meta.env.DEV) {
    // dev 兜底: 未登录时用 X-Mock-* 直连 (后端 dev_mock_header=true 时生效)。
    Object.entries(DEV_HEADERS).forEach(([k, v]) => {
      cfg.headers.set(k, v)
    })
  }
  cfg.headers.set('X-Client', 'admin-web')
  cfg.headers.set('X-Trace-Id', genTraceId())
  return cfg
})

// 响应拦截: 解 envelope (返回 data 部分, 改写 AxiosResponse.data 为 envelope.data)
client.interceptors.response.use(
  (res: AxiosResponse) => {
    const body = (res.data ?? {}) as Envelope<unknown>
    const code = typeof body.code === 'number' ? body.code : -1
    if (code === 0) {
      // 返回 data 部分 (与 api.get<T> 泛型约定一致: axios 默认返 res.data, 这里再解一层 envelope)
      return body.data as unknown as AxiosResponse
    }
    // 鉴权失效 (BE-A-01 真登录: token 过期/无效)
    if (code === 10001 || code === 10002 || code === 10003 || code === 40101) {
      clearToken()
      message.error('登录已失效，请重新登录')
      redirectToLogin()
      return Promise.reject(new ApiError(code, body.message || '登录已失效', body.trace_id))
    }
    // 业务/系统错误: 中文映射
    let msg = ERR_CODE_MSG[code]
    if (!msg) {
      if (code >= 50000) msg = FALLBACK_SYSTEM_MSG
      else msg = body.message || '请求失败'
    }
    message.error(msg)
    return Promise.reject(new ApiError(code, msg, body.trace_id))
  },
  (err) => {
    // HTTP 层错误 (非 2xx)
    const status = err.response?.status
    if (status && status >= 500) {
      message.error('服务开小差, 请稍后重试')
    } else if (status === 401) {
      clearToken()
      message.error('登录已失效，请重新登录')
      redirectToLogin()
    } else if (err.code === 'ECONNABORTED') {
      message.error('请求超时, 请重试')
    } else if (!err.response) {
      message.error('网络异常, 请检查后端服务是否启动 (:8080)')
    } else {
      message.error(err.message || '请求失败')
    }
    return Promise.reject(err instanceof ApiError ? err : new ApiError(-1, err.message || '网络异常'))
  },
)

// === 便捷方法 (返回 data 部分, 类型由调用方指定) ===
export const api = {
  get: <T>(url: string, config?: AxiosRequestConfig): Promise<T> =>
    client.get<unknown, T>(url, config),
  post: <T>(url: string, data?: unknown, config?: AxiosRequestConfig): Promise<T> =>
    client.post<unknown, T>(url, data, config),
  put: <T>(url: string, data?: unknown, config?: AxiosRequestConfig): Promise<T> =>
    client.put<unknown, T>(url, data, config),
  del: <T>(url: string, config?: AxiosRequestConfig): Promise<T> =>
    client.delete<unknown, T>(url, config),
}

export default client
