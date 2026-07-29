// services/coupon.ts — 券包服务 (真实: GET /v1/wxshop/coupons?status=)
//
// USE_MOCK=false: 接后端 #1。失败时降级回 mock (网络抖动/后端未起时离线演示可用)。
// MOCK_COUPONS 保留作离线演示与降级兜底,不删。

import { http } from '../utils/request'
import type { Coupon } from '../types/api'

const USE_MOCK = false // 后端 GET /v1/wxshop/coupons 已就绪 (CONTRACT §16)

// 内置示例券,覆盖 unused/used/expired 三态 (离线演示 / 真接口失败兜底)
const MOCK_COUPONS: Coupon[] = [
  { id: 1, title: '满50减10', type: 'cash', value_text: '¥10', threshold_text: '满¥50可用', scope_text: '全场通用', expire_text: '2026-08-31 到期', status: 'unused' },
  { id: 2, title: '满100减25', type: 'cash', value_text: '¥25', threshold_text: '满¥100可用', scope_text: '全场通用', expire_text: '2026-08-15 到期', status: 'unused' },
  { id: 3, title: '全场 8.8 折', type: 'discount', value_text: '8.8折', threshold_text: '无门槛', scope_text: '全场通用', expire_text: '2026-08-10 到期', status: 'unused' },
  { id: 4, title: '新人专享 满30减10', type: 'cash', value_text: '¥10', threshold_text: '满¥30可用', scope_text: '全场通用', expire_text: '2026-08-05 到期', status: 'unused' },
  { id: 5, title: '生日礼 免费咖啡', type: 'gift', value_text: '赠品', scope_text: '指定商品', expire_text: '2026-08-20 到期', status: 'unused' },
  { id: 6, title: '满80减15', type: 'cash', value_text: '¥15', threshold_text: '满¥80可用', scope_text: '全场通用', expire_text: '2026-07-20 到期', status: 'used' },
  { id: 7, title: '全场 9 折', type: 'discount', value_text: '9折', threshold_text: '无门槛', scope_text: '全场通用', expire_text: '2026-07-18 到期', status: 'used' },
  { id: 8, title: '满40减8', type: 'cash', value_text: '¥8', threshold_text: '满¥40可用', scope_text: '全场通用', expire_text: '2026-06-30 到期', status: 'expired' },
  { id: 9, title: '满200减60', type: 'cash', value_text: '¥60', threshold_text: '满¥200可用', scope_text: '全场通用', expire_text: '2026-06-15 到期', status: 'expired' },
]

/**
 * 券包列表 (GET /v1/wxshop/coupons?status=)。status 缺省返回 unused。
 * 真接口失败时降级回 mock 过滤 (兜底,不阻断页面)。
 */
export function listCoupons(status?: 'unused' | 'used' | 'expired'): Promise<Coupon[]> {
  const want = status || 'unused'
  if (USE_MOCK) {
    return Promise.resolve(MOCK_COUPONS.filter((c) => c.status === want))
  }
  const qs = status ? `?status=${status}` : ''
  return http.get<Coupon[]>(`/v1/wxshop/coupons${qs}`).catch((err) => {
    // 降级: 真接口失败 → 回 mock 对应状态 (离线/后端未起兜底)
    console.warn('[coupon] listCoupons 真接口失败,降级 mock', err)
    return MOCK_COUPONS.filter((c) => c.status === want)
  })
}
