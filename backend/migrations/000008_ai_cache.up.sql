-- ============================================================
-- 000008_ai_cache.up.sql
-- AI 引擎 L2 语义缓存 (M3-S2, AI-06)
-- 依据: 技术方案 第7章 §7.6.3 (语义缓存 DDL, 行 682-701)
--       CONTRACT §13.2 / §13.3.4 (M3-S2 迁移编号与接口)
-- 目标数据库: PostgreSQL 16 + pgvector 扩展
--
-- 范围(对照 CONTRACT §13.2):
--   单表 ai_cache: 存当日 Advice 草稿的 embedding + advice_json,
--   供 PGVectorSemanticCache 做余弦近邻检索(相似度 >0.85 且未过期
--   且同业态)。凌晨预生成(M3-S2 AI-07)写入; Pipeline L2 命中读取。
--
-- 命名说明: ch05 §5.3.9 的 strategy_cache 是旧命名, 统一用 ai_cache
--   (CONTRACT §13.2 权威)。
--
-- 扩展: CREATE EXTENSION IF NOT EXISTS vector (幂等, 需 PG 镜像内置
--   pgvector; 生产运维确认镜像版本)。
--
-- 维度: embedding vector(1024) 固定 (bge-m3 / text-embedding-v3,
--   CONTRACT §13.5.6)。
--
-- 索引:
--   * HNSW 余弦近邻索引 (m=16, ef_construction=64) 加速 <=> 检索。
--   * btree(tenant_id, business, expires_at) 加速同业态未过期过滤 +
--     Invalidate 的 UPDATE ... WHERE tenant_id=$1。
--
-- 失效策略(§7.6.4):
--   * TTL: expires_at = created_at + 24h (凌晨预生成当天有效)。
--   * 主动失效: UPDATE expires_at=now() WHERE tenant_id=$1
--     (商户执行动作后, PGVectorSemanticCache.Invalidate 调)。
--
-- 触发器: 无 updated_at, 不挂 trg_set_updated_at (语义缓存是 append/
--   invalidate 语义, 非实体生命周期)。
-- ============================================================

CREATE EXTENSION IF NOT EXISTS vector;


-- ============================================================
-- ai_cache : L2 语义缓存表
--    embedding  vector(1024): signal_text 的 1024 维 embedding。
--    advice_json JSONB:        完整 *strategy.Strategy 快照。
--    key_text   TEXT:          构造的 signal_text 文本(BuildSignalText),
--                              便于排查/重算, 不参与检索(检索走 embedding)。
--    hit_count  INT:           命中计数(本期 SELECT 不自增, 预留观测)。
--    tenant_id 全部 NOT NULL REFERENCES tenants(id), 行级隔离键。
-- ============================================================
CREATE TABLE ai_cache (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   BIGINT       NOT NULL REFERENCES tenants(id),
    business    TEXT         NOT NULL,
    key_text    TEXT         NOT NULL,
    embedding   vector(1024) NOT NULL,
    advice_json JSONB        NOT NULL,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ  NOT NULL,
    hit_count   INT          NOT NULL DEFAULT 0
);

-- HNSW 余弦近邻索引 (pgvector ≥0.5, 需核实镜像内置版本)。
CREATE INDEX ai_cache_embedding_idx ON ai_cache
    USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 64);

-- 同业态 + 未过期过滤 + Invalidate 的 tenant 范围 UPDATE。
CREATE INDEX ai_cache_tenant_business_expires_idx
    ON ai_cache (tenant_id, business, expires_at);

COMMENT ON TABLE  ai_cache                    IS 'L2 语义缓存(当日 Advice embedding 近邻检索); ch07 §7.6.3; Invalidate 走 UPDATE expires_at=now(); 无 updated_at';
COMMENT ON COLUMN ai_cache.id                 IS '自增主键(GENERATED ALWAYS AS IDENTITY)';
COMMENT ON COLUMN ai_cache.tenant_id          IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN ai_cache.business           IS '业态: fruit|convenience|maternity|fresh|apparel, 检索时同业态过滤收紧语义空间';
COMMENT ON COLUMN ai_cache.key_text           IS '构造的 signal_text 文本(BuildSignalText), 排查/重算用, 不参与检索';
COMMENT ON COLUMN ai_cache.embedding          IS 'signal_text 的 1024 维 embedding (bge-m3/text-embedding-v3), vector(1024)';
COMMENT ON COLUMN ai_cache.advice_json        IS '完整 *strategy.Strategy 快照 JSONB';
COMMENT ON COLUMN ai_cache.created_at         IS '写入时间(UTC); 凌晨预生成或 Pipeline 首算写入';
COMMENT ON COLUMN ai_cache.expires_at         IS '过期时间(UTC); created_at + TTL(默认 24h); 过期不命中; Invalidate 置 now() 主动失效';
COMMENT ON COLUMN ai_cache.hit_count          IS '命中计数(本期 SELECT 不自增, 预留观测/反例降权 M3-S3)';
