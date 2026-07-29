// services/order.ts — 订单服务 (真实: GET /v1/wxshop/orders)
//
// USE_MOCK=false: 接后端 #7 (billing.ListByMember, CONTRACT §16.3(7)/§16.8)。
// 后端返真实单 (dev fixture billing 已装配); 返空数组则页面空态;
// 真接口失败降级回空数组 (读操作, 不阻断页面)。

import { http } from '../utils/request'
import type { Order } from '../types/api'

const USE_MOCK = false // 后端 GET /v1/wxshop/orders 已就绪 (CONTRACT §16.3(7)/§16.8)

/** 订单/交易记录列表 (GET /v1/wxshop/orders; 失败降级回空数组, 页面空态) */
export function listOrders(): Promise<Order[]> {
  if (USE_MOCK) {
    return Promise.resolve([])
  }
  return http.get<Order[]>('/v1/wxshop/orders').catch((err) => {
    // 降级: 真接口失败 → 回空数组 (页面空态, 不阻断)
    console.warn('[order] listOrders 真接口失败,降级空态', err)
    return [] as Order[]
  })
}
