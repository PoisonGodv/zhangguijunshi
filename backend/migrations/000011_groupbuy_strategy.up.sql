-- ============================================================
-- 000011_groupbuy_strategy.up.sql
-- M3-S3 收尾: groupbuy → strategy 归因链路
-- 依据: CONTRACT §14.3.1 GroupbuyStats 端口 StartedSucceeded(ctx, tenantID, strategyID)
--       归因 key 冻结在 strategy_id 粒度。本迁移给 groupbuys 补 strategy_id 列,
--       让 groupbuy_success 归因 metric 真实计算(M3-S3 之前 groupbuyStatsAdapter
--       在 dev 返 (0,0,nil), Computer 跳过 groupbuy_success)。
--
-- 纯 additive: 不改既有列, 不删既有索引/触发器。strategy_id 默认 0 = 非AI来源,
-- 与 reach_tasks.strategy_id (nullable) 语义对齐(此处用 NOT NULL DEFAULT 0
-- 因为 groupbuys 无 NULL 归因场景; 0 即未归因)。
-- ============================================================

ALTER TABLE groupbuys ADD COLUMN strategy_id BIGINT NOT NULL DEFAULT 0;

CREATE INDEX idx_groupbuys_strategy ON groupbuys(tenant_id, strategy_id);

COMMENT ON COLUMN groupbuys.strategy_id IS 'AI策略项ID(归因键), 0=非AI来源; StartedSucceeded 据此计算 groupbuy_success 归因 metric; M3 建表后补外键';
