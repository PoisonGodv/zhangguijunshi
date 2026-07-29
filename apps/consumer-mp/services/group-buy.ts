// services/group-buy.ts — 拼团服务 (真实: /v1/wxshop/groupbuys/active + /:id/join)
//
// USE_MOCK=false: 接后端 #5/#6。列表失败时降级回 mock (离线/后端未起兜底);
// 参团失败不降级 (参团是写操作,降级会造成"假参团"误导,直接抛错由页面 toast)。

import { http } from '../utils/request'
import type { GroupBuyBrief } from '../types/api'

const USE_MOCK = false // 后端 GET /v1/wxshop/groupbuys/active 已就绪 (CONTRACT §16)

// 内置示例拼团 (离线演示 / 列表真接口失败兜底)
const MOCK_GROUP_BUYS: GroupBuyBrief[] = [
  { id: 101, title: '3人团 新鲜草莓 2斤装', price_cents: 1990, original_cents: 2990, required: 3, joined: 2 },
  { id: 102, title: '5人团 进口牛奶一箱', price_cents: 4590, original_cents: 6900, required: 5, joined: 1 },
]

/** 进行中拼团列表 (GET /v1/wxshop/groupbuys/active);失败降级 mock */
export function listGroupBuys(): Promise<GroupBuyBrief[]> {
  if (USE_MOCK) {
    return Promise.resolve(MOCK_GROUP_BUYS)
  }
  return http.get<GroupBuyBrief[]>('/v1/wxshop/groupbuys/active').catch((err) => {
    // 降级: 真接口失败 → 回 mock 列表 (离线/后端未起兜底)
    console.warn('[group-buy] listGroupBuys 真接口失败,降级 mock', err)
    return MOCK_GROUP_BUYS
  })
}

/**
 * 参团 (POST /v1/wxshop/groupbuys/:id/join)。写操作不降级,失败直接抛 ApiError:
 *   60013 拼团已满 / 60014 拼团已结束 / 60015 已参团 (request.ts ERR_CODE_MSG 已映射中文)。
 */
export function joinGroupBuy(id: number): Promise<unknown> {
  if (USE_MOCK) {
    return Promise.resolve({ ok: true, mock: true })
  }
  return http.post<unknown>(`/v1/wxshop/groupbuys/${id}/join`)
}
