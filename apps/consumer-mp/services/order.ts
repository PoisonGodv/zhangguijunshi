// services/order.ts — 订单服务 (真实: GET /v1/wxshop/orders)
//
// USE_MOCK=false: 接后端 #7 (billing.ListByMember, CONTRACT §16.3(7)/§16.8) + 订单详情 (billing.GetOrder, CONTRACT §24.3)。
// 后端返真实单 (dev fixture billing 已装配); 返空数组则页面空态;
// 列表真接口失败降级回空数组 (读操作, 不阻断页面); 详情失败降级 null (页面 toast 退回)。

import { http } from '../utils/request'
import type { Order, OrderDetail } from '../types/api'

const USE_MOCK = false // 后端 GET /v1/wxshop/orders 已就绪 (CONTRACT §16.3(7)/§16.8); /:id 详见 §24.3

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

/**
 * 订单详情 (GET /v1/wxshop/orders/:id; CONTRACT §24.3)。
 * 读操作: 真接口失败降级回 null, 页面 toast 退回 (不阻断)。
 * 跨租户/非本人订单 → 后端 40404 (与"不存在"不可区分), 这里统一降级 null。
 */
export async function getOrderDetail(id: number): Promise<OrderDetail | null> {
  if (USE_MOCK) {
    return null
  }
  try {
    // URL 用字符串拼接 (避免模板字符串反引号与 workflow 脚本冲突)
    return await http.get<OrderDetail>('/v1/wxshop/orders/' + id)
  } catch (err) {
    // 降级: 真接口失败 → 返回 null (页面 toast 退回, 不阻断)
    console.warn('[order] getOrderDetail 真接口失败,降级 null', err)
    return null
  }
}
