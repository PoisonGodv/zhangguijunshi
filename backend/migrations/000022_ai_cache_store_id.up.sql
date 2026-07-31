-- ============================================================
-- 000022_ai_cache_store_id.up.sql
-- L2 语义缓存检索键补 store_id (aiengine 三合一 修复①)
-- 依据: CONTRACT §13.3.4 / 技术方案 ch07 §7.6.4
--
-- 背景(高危缺陷):
--   ai_cache 原检索键仅 (tenant_id, business, embedding 近邻, expires_at)。
--   多店租户的两家门店若当日信号语义近似(同业态+同天气+同临期结构),
--   PGVectorSemanticCache.TryHit 会用 A 店的 embedding 命中 B 店写入的
--   advice_json —— 而 advice 里绑定的是 B 店的商品/会员, 等于把 B 店的
--   建议串给 A 店(跨店越权, 类比跨租户串户)。
--
-- 修复:
--   1) 新增 store_id BIGINT NOT NULL DEFAULT 0 (存量行回填 0, 与
--      Memory 实现 memEntry.storeID 的零值语义一致; 0 = 未分店的
--      legacy/dev 草稿)。不加外键: ai_cache 是 append/invalidate 语义
--      的缓存表, 门店删除不应级联清缓存(与 000008 的 tenant_id 仅引用
--      tenants 保持一致的保守外键策略)。
--   2) 检索/写入 SQL 由 PGVectorSemanticCache 带上 store_id 过滤
--      (WHERE ... AND store_id=$2 / INSERT 含 store_id 列)。
--   3) 复合索引 (tenant_id, store_id, business, expires_at) 替代原
--      (tenant_id, business, expires_at), 收紧同店同业态未过期过滤。
--      HNSW embedding 近邻索引不变。
--
-- 幂等: 列/索引均 IF NOT EXISTS, 可安全重跑。
-- ============================================================

ALTER TABLE ai_cache
    ADD COLUMN IF NOT EXISTS store_id BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN ai_cache.store_id IS '所属门店ID(检索键之一, 多店隔离); 0=未分店 legacy/dev 草稿; 不加外键(缓存表 append/invalidate 语义, 门店删除不级联)';

-- 替代原 (tenant_id, business, expires_at): 收紧到同店同业态未过期。
DROP INDEX IF EXISTS ai_cache_tenant_business_expires_idx;

CREATE INDEX IF NOT EXISTS ai_cache_tenant_store_business_expires_idx
    ON ai_cache (tenant_id, store_id, business, expires_at);
