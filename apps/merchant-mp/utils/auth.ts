// utils/auth.ts — 鉴权高层封装 (给页面用),对齐 consumer-mp/utils/auth.ts 薄封装范式。
//
// 页面/服务层不直接碰 storage key;token 存储 key (zgjs_access/zgjs_refresh) 由 request.ts 统一管理。
// B 端登录是 boss 员工账密登录 (POST /v1/boss/auth/login),不是 consumer-mp 的 wx.login。

import type { LoginResp } from '../types/api'
import { bossLogin, logoutReq, getAccessToken, clearTokens } from './request'

/** 是否已登录 (本地存在 access token;不校验有效期,过期由请求层 refresh 兜底) */
export function isLoggedIn(): boolean {
  return !!getAccessToken()
}

/** 当前 access token (无则空串) / 清空双 token */
export { getAccessToken, clearTokens }

/** 触发登录: boss 员工账密 → 后端换 JWT → 存 token,返回原始响应 */
export async function doLogin(tenantID: number, phone: string, password: string): Promise<LoginResp> {
  return bossLogin(tenantID, phone, password)
}

/** 登出: 通知后端撤销 refresh/block jti,并清空本地 token (本地失败也清) */
export async function logout(): Promise<void> {
  return logoutReq()
}
