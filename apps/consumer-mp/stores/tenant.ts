// stores/tenant.ts — 当前租户/门店 store (FE-MP-01)
//
// tenantID 默认 1 (开发态),进店扫码 / 门店码分流后更新 storeID/storeName。

import { createStore } from './store'

export interface TenantState {
  tenantID: number
  storeID: number
  storeName?: string
}

export const tenantStore = createStore<TenantState>({
  tenantID: 1,
  storeID: 0,
})

/** 从 globalData 同步 tenantID 进 store (app onLaunch 后调用一次) */
export function initTenantFromGlobal(): void {
  const app = getApp() as unknown as { globalData?: { tenantID?: number } } | undefined
  const tid = app && app.globalData && typeof app.globalData.tenantID === 'number'
    ? app.globalData.tenantID
    : 1
  tenantStore.set({ tenantID: tid })
}
