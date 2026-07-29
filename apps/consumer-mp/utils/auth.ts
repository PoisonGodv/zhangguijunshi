// utils/auth.ts — 鉴权高层封装 (给 store/page 用)
//
// 薄封装 utils/request.ts 的 token 读写与 login/logout,页面/服务层不直接碰 storage key。
// token 存储 key (zgjs_access/zgjs_refresh) 由 request.ts 统一管理,这里复用其导出函数。

import type { WxLoginResp } from '../types/api'
import { wxLogin, logoutReq, getAccessToken, clearTokens } from './request'

/** 是否已登录 (本地存在 access token;不校验有效期,过期由请求层 refresh 兜底) */
export function isLoggedIn(): boolean {
  return !!getAccessToken()
}

/** 当前 access token (无则空串) */
export { getAccessToken, clearTokens }

/** 触发登录: wx.login → 后端换 JWT → 存 token,返回原始响应 */
export async function doLogin(): Promise<WxLoginResp> {
  return wxLogin()
}

/** 登出: 通知后端撤销 refresh/block jti,并清空本地 token */
export async function logout(): Promise<void> {
  return logoutReq()
}
