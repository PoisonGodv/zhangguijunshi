// stores/user.ts — 会员状态 store (FE-MP-01)
//
// 跨页共享: 会员信息 / access / refresh token。页面用 userStore.subscribe / userStore.get。
// 修改不挂到 store 对象上 (store 是纯 {get,set,subscribe}),用下方独立函数。

import { createStore } from './store'
import type { MemberInfo } from '../types/api'

export interface UserState {
  member: MemberInfo | null
  accessToken: string
  refreshToken: string
}

export const userStore = createStore<UserState>({
  member: null,
  accessToken: '',
  refreshToken: '',
})

/** 写入 token (登录/刷新后调用) */
export function setUserTokens(access: string, refresh: string): void {
  userStore.set({ accessToken: access, refreshToken: refresh })
}

/** 写入会员信息 */
export function setUserMember(m: MemberInfo): void {
  userStore.set({ member: m })
}

/** 清空 (登出) */
export function clearUser(): void {
  userStore.set({ member: null, accessToken: '', refreshToken: '' })
}
