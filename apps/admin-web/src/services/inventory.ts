// src/services/inventory.ts — 库存域 boss 接口封装 (FE-ADMIN-02)
//
// 对齐 backend/interfaces/http/handler/inventory.go + internal/inventory/inventory.go:
//   - Batch / Movement 结构体无 json tag → PascalCase 字段名本身。
//   - GET /inventory/stock 响应为 handler 内联 gin.H{"product_id":..,"store_id":..,
//     "quantity":..} (snake_case 键; quantity 小写, CONTRACT §17 提醒) — 仅返某商品
//     当前总余量, 非批次明细。
//   - GET /inventory/movements 直接返 []Movement (无包装, 无 total) → 前端全量拉取分页。
//   - CONTRACT §24.2 起新增 GET /inventory/batches → {items:[Batch], total:int64}
//     (handler 层 DTO snake_case 键; Batch 实体 PascalCase), 临期看板改用真实批次。

import { api } from '@/services/request'

/** 库存批次 (inventory.Batch, PascalCase) */
export interface Batch {
  ID: number
  TenantID: number
  StoreID: number
  ProductID: number
  Quantity: number // 当前余量
  InPriceCents: number
  ExpiryDate: string | null
  ExpiryWarnDate: string | null
  Status: string // active | nearing | expired | cleared
  CreatedAt: string
  UpdatedAt: string
}

/** 库存流水 (inventory.Movement, PascalCase; Quantity 正入负出) */
export interface Movement {
  ID: number
  TenantID: number
  StoreID: number
  ProductID: number
  BatchID: number // 0 = 无关联批次
  Type: string // purchase_in | sale_out | stocktake_adj | return_in | loss | transfer
  Quantity: number // 正入负出
  RefOrderID: number // 0 = 无关联订单
  OperatorID: number
  Note: string
  CreatedAt: string
}

/** GetStock 响应 (handler gin.H, snake_case 键; quantity 小写) */
export interface StockResp {
  product_id: number
  store_id: number
  quantity: number
}

/** 入库请求体 (handler purchaseInRequest, snake_case; product_id/quantity 必填) */
export interface PurchaseBody {
  store_id?: number
  product_id: number
  quantity: number
  in_price_cents?: number
  expiry_date?: string // YYYY-MM-DD, 可选
  note?: string
}

/** 消耗扣库存请求体 (handler consumeRequest, snake_case; qty 必填) */
export interface ConsumeBody {
  store_id?: number
  qty: number
  ref_order_id?: number
}

export interface MovementsParams {
  store_id?: number
  product_id?: number
  type?: string
  date?: string // YYYY-MM-DD
  limit?: number
  offset?: number
}

/** ListBatches 查询参数 (CONTRACT §24.2, snake_case query) */
export interface BatchesParams {
  store_id?: number
  product_id?: number
  status?: string // active | nearing | expired | cleared
  expiry_before?: string // YYYY-MM-DD, 临期看板筛选用
  limit?: number
  offset?: number
}

/** ListBatches 响应 (handler gin.H 包装, snake_case 键; items 为 Batch 实体 PascalCase) */
export interface BatchesListResp {
  items: Batch[]
  total: number
}

/** GET /v1/boss/inventory/stock?product_id=&store_id= — 查商品当前总库存 */
export function getStock(productId: number, storeId?: number): Promise<StockResp> {
  return api.get<StockResp>('/boss/inventory/stock', {
    params: { product_id: productId, store_id: storeId },
  })
}

/** POST /v1/boss/inventory/purchase — 入库 (返新批次) */
export function purchase(body: PurchaseBody): Promise<Batch> {
  return api.post<Batch>('/boss/inventory/purchase', body)
}

/** GET /v1/boss/inventory/movements — 库存流水 (返全量, 无 total) */
export function listMovements(params?: MovementsParams): Promise<Movement[]> {
  return api.get<Movement[]>('/boss/inventory/movements', { params })
}

/** GET /v1/boss/inventory/batches — 批次列表 (CONTRACT §24.2, 返 {items,total}) */
export function listBatches(params?: BatchesParams): Promise<BatchesListResp> {
  return api.get<BatchesListResp>('/boss/inventory/batches', { params })
}

/** POST /v1/boss/inventory/products/:id/consume — FIFO 扣库存 (返被扣批次) */
export function consume(productId: number, body: ConsumeBody): Promise<Batch[]> {
  return api.post<Batch[]>(`/boss/inventory/products/${productId}/consume`, body)
}
