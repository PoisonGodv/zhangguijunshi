// services/store.ts — 门店服务 (真实: GET /v1/wxshop/stores; CONTRACT §16.3(11))
//
// 后端 store.Service 装配时返真实门店列表 (tenant 作用域 + active 过滤);
// 未装配 (minimal fixture) 后端降级返空数组 (不 404)。
// 读操作: 真接口失败降级回空数组 (页面空态, 不阻断)。

import { http } from '../utils/request'
import type { Store } from '../types/api'

const USE_MOCK = false // 后端 GET /v1/wxshop/stores 已就绪 (CONTRACT §16.3(11))

// 内置示例门店 (离线演示 / 真接口失败兜底)
const MOCK_STORES: Store[] = [
  { id: 1, name: '掌柜优选·幸福里店', address: '幸福里社区商业街 12 号', phone: '0571-88888001', group_qr: 'https://example.com/qr/xingfuli.png', active: true },
  { id: 2, name: '掌柜优选·城东店', address: '城东大道 88 号', phone: '0571-88888002', active: true },
]

/**
 * 门店列表 (GET /v1/wxshop/stores)。失败降级回空数组 (页面空态, 不阻断)。
 */
export function listStores(): Promise<Store[]> {
  if (USE_MOCK) {
    return Promise.resolve(MOCK_STORES)
  }
  return http.get<Store[]>('/v1/wxshop/stores').catch((err) => {
    // 降级: 真接口失败 → 回空数组 (页面空态, 不阻断)
    console.warn('[store] listStores 真接口失败,降级空态', err)
    return [] as Store[]
  })
}
