// services/auth.ts — C 端鉴权 API (后端 /v1/wxshop/auth 真实可用)
//
// 仅 auth 是真实接口;业务接口走各服务的 mock fallback。

import { http, getRefreshToken, ApiError, appGlobal } from '../utils/request'
import type { WxLoginResp, MeResp, Envelope } from '../types/api'

/** 登录: POST /v1/wxshop/auth/login {code, tenant_id} → WxLoginResp */
export function login(code: string, tenantID: number): Promise<WxLoginResp> {
  return http.post<WxLoginResp>('/v1/wxshop/auth/login', { code, tenant_id: tenantID })
}

/** 当前身份: GET /v1/wxshop/auth/me (Bearer) → MeResp */
export function me(): Promise<MeResp> {
  return http.get<MeResp>('/v1/wxshop/auth/me')
}

/** 登出: POST /v1/wxshop/auth/logout */
export function logout(): Promise<void> {
  return http.post<void>('/v1/wxshop/auth/logout')
}

/**
 * 刷新: POST /v1/wxshop/auth/refresh,需在 X-Refresh-Token 头带 refresh_token。
 * 注意: utils/request.ts 的 http 封装会固定注入 Bearer access,不支持自定义
 * X-Refresh-Token 头,故此处直接 wx.request 调用该端点 (与 request.ts 内部 refreshToken 同源逻辑,
 * 供服务层显式刷新用)。
 */
export function refresh(): Promise<WxLoginResp> {
  const refreshToken = getRefreshToken()
  if (!refreshToken) return Promise.reject(new ApiError(10002, '登录已过期'))
  return new Promise<WxLoginResp>((resolve, reject) => {
    wx.request({
      url: appGlobal().baseURL + '/v1/wxshop/auth/refresh',
      method: 'POST',
      timeout: 10000,
      header: {
        'Content-Type': 'application/json',
        'X-Client': 'consumer-mp',
        'X-Refresh-Token': refreshToken,
      },
      success: (r) => {
        const body = (r.data as Envelope<WxLoginResp>) || ({ code: -1 } as Envelope<WxLoginResp>)
        if (r.statusCode === 200 && body.code === 0 && body.data) {
          resolve(body.data)
          return
        }
        const code = typeof body.code === 'number' ? body.code : -1
        reject(new ApiError(code, body.message || '续期失败', body.trace_id))
      },
      fail: (err) => reject(new ApiError(-1, err.errMsg || '网络异常')),
    })
  })
}
