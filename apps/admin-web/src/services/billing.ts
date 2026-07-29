// src/services/billing.ts — 经营流水/看板 boss 接口封装 (FE-ADMIN-02)
//
// 后端 CONTRACT §8: GET /v1/boss/billing/summary/today 与 /:date。
// billing.DailySummary / billing.TopProduct 为 PascalCase (Go 结构体无 json tag,
// 序列化为字段名本身), 故本文件类型保持 PascalCase 与之对齐。
//
// 字段语义 (经 internal/billing/service.go DailySummary 核实):
//   GrossCents  = Σ order.TotalCents  (营业额/流水)
//   NetCents    = Σ order.PaidCents   (实收; 已付单 total==paid, 故与 gross 相等)
//   CostCents   = Σ qty*CostCents     (成本)
//   ProfitCents = NetCents - CostCents(毛利)
//   TopProducts 按 qty desc 排序 (最多 5 条), GrossCents = Σ qty*PriceCents。
// 当日无已付订单时后端返回零值汇总 (非 404), 趋势拉取不会触发错误 toast。

import { api } from './request'

/** 热销商品行 (billing.TopProduct, PascalCase) */
export interface TopProduct {
  ProductID: number
  Name: string
  Qty: number
  GrossCents: number // 该商品营收 (分)
}

/** 当日流水摘要 (billing.DailySummary, PascalCase) */
export interface DailySummary {
  TenantID: number
  StoreID: number
  Date: string // YYYY-MM-DD
  OrderCount: number
  GrossCents: number // 营业额 (分)
  NetCents: number // 实收 (分)
  CostCents: number // 成本 (分)
  ProfitCents: number // 毛利 (分)
  TopProducts: TopProduct[]
}

/** GET /v1/boss/billing/summary/today — 当日流水 */
export function dailySummaryToday(): Promise<DailySummary> {
  return api.get<DailySummary>('/boss/billing/summary/today')
}

/** GET /v1/boss/billing/summary/:date — 指定日期流水 (date = YYYY-MM-DD) */
export function dailySummary(date: string): Promise<DailySummary> {
  return api.get<DailySummary>(`/boss/billing/summary/${date}`)
}
