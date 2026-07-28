// app.ts — 商家小程序入口 (FE-MP-03)
// globalData: 后端 baseURL + 多租户鉴权头 (M0-S2 前用 X-Mock-Tenant,FE-MP-02 接 JWT 后改 utils/request.ts 的 attachAuth)

interface AppGlobalData {
  baseURL: string
  tenantHeader: Record<string, string>
}

App({
  globalData: {
    baseURL: 'http://localhost:8080',
    tenantHeader: { 'X-Mock-Tenant': '1' },
  } as AppGlobalData,
  onLaunch() {
    // M0-S2 JWT 接入后:读本地 access token,在 attachAuth 注入 Authorization: Bearer <jwt>
  },
})
