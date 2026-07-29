// app.ts — 消费者端小程序入口 (FE-MP-01)
//
// 鉴权说明: C 端 JWT (wx.login → POST /v1/wxshop/auth/login) 由 utils/request.ts 统一管理,
//           app.ts 不直接处理 token。globalData 只持有 baseURL 与 tenantID。
// tenantID:  开发态默认 1;未来由进店扫码 / 企微活码绑定具体租户 (技术方案 §9.2.5)。
// 埋点:      onLaunch 劫持全局 Page,包裹 onShow/onHide 注入页面停留埋点 (技术方案 §9.2.6 范式)。

import { tracker } from './utils/tracker'

interface AppGlobalData {
  baseURL: string
  tenantID: number
}

App({
  globalData: {
    baseURL: 'http://localhost:8080',
    tenantID: 1,
  } as AppGlobalData,

  onLaunch() {
    // 劫持全局 Page:包裹每个页面的 onShow/onHide,注入页面停留埋点 (技术方案 §9.2.6)
    const globalAny = global as any
    const originalPage = globalAny.Page
    globalAny.Page = function (opts: Record<string, any>) {
      const originalOnShow = opts.onShow
      const originalOnHide = opts.onHide

      opts.onShow = function (this: { route: string }, ...args: unknown[]): unknown {
        tracker.onPageShow(this.route)
        if (originalOnShow) return originalOnShow.apply(this, args)
      }

      opts.onHide = function (this: unknown, ...args: unknown[]): unknown {
        tracker.onPageHide()
        if (originalOnHide) return originalOnHide.apply(this, args)
      }

      // 保留原 Page 的返回值
      return originalPage(opts)
    }
  },
})
