// app.ts — 商家小程序入口
// globalData: 后端 baseURL + X-Mock-Tenant dev 兜底头 (prod 走 JWT Bearer,见 utils/request.ts attachAuth)。
// 登录页只在「无 token 或 refresh 也失败」时出现,onLaunch 不做强制登录,由各页面 ensureToken/捕获 10002 触发。

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
    // 鉴权由各页面惰性触发: utils/request.ts 401/10001/10002 → refresh → 失败抛 10002,页面跳登录页
  },
})
