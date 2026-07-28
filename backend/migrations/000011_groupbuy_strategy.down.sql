-- ============================================================
-- 000011_groupbuy_strategy.down.sql
-- 回滚 000011: 撤掉 groupbuys.strategy_id 列及其索引。
-- 纯 additive 列的回滚安全(无既有依赖)。
-- ============================================================

DROP INDEX IF EXISTS idx_groupbuys_strategy;

ALTER TABLE groupbuys DROP COLUMN IF EXISTS strategy_id;
