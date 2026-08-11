// src/services/catalog.ts — 商品域 boss 接口封装 (FE-ADMIN-02)
//
// 对齐 backend/interfaces/http/handler/catalog.go + internal/catalog/catalog.go:
//   - Product 结构体无 json tag → 序列化为 PascalCase 字段名本身。
//   - 金额字段 CostCents/PriceCents 单位为分 (int64)。
//   - ListProducts 响应为 handler 内联 gin.H{"items":..,"total":..} (snake_case 键;
//     CONTRACT §24.5 起 count(本次条数) → total(全量真分页))。
//   - UpdateProductPrice 走 PATCH (request.ts 的 api 仅暴露 get/post/put/del,
//     这里复用 default client 的 patch, 与响应拦截器同一 envelope 解包链路)。

import { api } from '@/services/request'
import client from '@/services/request'

/** 商品 (catalog.Product, PascalCase) */
export interface Product {
  ID: number
  TenantID: number
  StoreID: number
  Barcode: string
  Name: string
  Category: string
  Unit: string
  CostCents: number
  PriceCents: number
  ImageURL: string
  ShelfLifeDays: number
  Tags: string[]
  Status: string // active | archived
  CreatedAt: string
  UpdatedAt: string
}

/** ListProducts 响应 (handler gin.H 包装, snake_case 键; CONTRACT §24.5: total=全量条数) */
export interface ProductListResp {
  items: Product[]
  total: number
}

export interface ProductListParams {
  store_id?: number
  query?: string // 名称/条码模糊
  category?: string
  limit?: number
  offset?: number
}

/** 新增商品请求体 (handler createProductRequest, snake_case; name 必填) */
export interface CreateProductBody {
  barcode?: string
  name: string
  category?: string
  unit?: string
  cost_cents?: number
  price_cents?: number
  image_url?: string
  shelf_life_days?: number
  tags?: string[]
}

/** 改价请求体 (handler updatePriceRequest, snake_case; price>=cost>=0 后端校验) */
export interface UpdatePriceBody {
  cost_cents: number
  price_cents: number
}

/** GET /v1/boss/catalog/products — 商品列表 (CONTRACT §24.5 起响应含真分页 total) */
export function getProducts(params?: ProductListParams): Promise<ProductListResp> {
  return api.get<ProductListResp>('/boss/catalog/products', { params })
}

/** POST /v1/boss/catalog/products — 新增商品 */
export function createProduct(body: CreateProductBody): Promise<Product> {
  return api.post<Product>('/boss/catalog/products', body)
}

/** GET /v1/boss/catalog/products/:id — 商品详情 */
export function getProduct(id: number): Promise<Product> {
  return api.get<Product>(`/boss/catalog/products/${id}`)
}

/** GET /v1/boss/catalog/products/barcode/:barcode — 按条码查 (扫码开单) */
export function getByBarcode(barcode: string): Promise<Product> {
  return api.get<Product>(`/boss/catalog/products/barcode/${encodeURIComponent(barcode)}`)
}

/** PATCH /v1/boss/catalog/products/:id/price — 改价 (price>=cost>=0) */
export function updatePrice(id: number, body: UpdatePriceBody): Promise<Product> {
  return client.patch<unknown, Product>(`/boss/catalog/products/${id}/price`, body)
}
