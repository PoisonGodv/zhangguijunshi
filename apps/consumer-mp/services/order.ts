// services/order.ts — 订单服务 (真实: GET /v1/wxshop/orders)
//
// USE_MOCK=false: 接后端 #7。后端本期降级返回空数组 [] (billing 无按 member 查询,
// CONTRACT §16.8 TODO), 前端显示空态即可;真接口失败降级回空数组。

import { http } from '../utils/request'

const USE_MOCK = false // 后端 GET /v1/wxshop/orders 已就绪 (本期返回空, CONTRACT §16.8)

/** 订单/交易记录列表 (GET /v1/wxshop/orders;后端本期返回空数组,页面空态) */
export function listOrders(): Promise<unknown[]> {
  if (USE_MOCK) {
    return Promise.resolve([])
  }
  return http.get<unknown[]>('/v1/wxshop/orders').catch((err) => {
    // 降级: 真接口失败 → 回空数组 (页面空态,不阻断)
    console.warn('[order] listOrders 真接口失败,降级空态', err)
    return [] as unknown[]
  })
}
