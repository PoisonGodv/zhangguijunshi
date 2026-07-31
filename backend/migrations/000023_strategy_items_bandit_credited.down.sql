-- ============================================================
-- 000023_strategy_items_bandit_credited.down.sql
-- 逆序回滚 000023_strategy_items_bandit_credited.up.sql
-- 幂等: 均 IF EXISTS。
-- ============================================================

DROP INDEX IF EXISTS idx_strategy_items_bandit_uncredited;

ALTER TABLE strategy_items DROP COLUMN IF EXISTS bandit_credited_at;
