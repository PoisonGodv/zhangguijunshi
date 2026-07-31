-- ============================================================
-- 000022_ai_cache_store_id.down.sql
-- 逆序回滚 000022_ai_cache_store_id.up.sql
--
-- 恢复原 (tenant_id, business, expires_at) 复合索引, 删除新增的
-- (tenant_id, store_id, business, expires_at) 索引与 store_id 列。
-- 幂等: 均 IF EXISTS / IF NOT EXISTS。
-- ============================================================

DROP INDEX IF EXISTS ai_cache_tenant_store_business_expires_idx;

CREATE INDEX IF NOT EXISTS ai_cache_tenant_business_expires_idx
    ON ai_cache (tenant_id, business, expires_at);

ALTER TABLE ai_cache DROP COLUMN IF EXISTS store_id;
