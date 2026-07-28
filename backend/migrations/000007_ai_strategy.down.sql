-- ============================================================
-- 000007_ai_strategy.down.sql
-- 回滚 000007: 逆序删除 AI 引擎/归因域 4 张表
--
-- 依赖关系(FK):
--   strategy_items.strategy_id        -> strategies.id           (子->父)
--   strategy_attribution.strategy_item_id -> strategy_items.id   (子->父)
--   strategy_feedback.strategy_item_id    -> strategy_items.id   (子->父)
--   其余基表(tenants/stores/products)属 000001-000003, 不在本迁移。
--
-- 删表顺序(先子后父):
--   strategy_feedback(孙) -> strategy_attribution(孙) ->
--   strategy_items(子)    -> strategies(父)
--
-- 触发器: 4 张表均无 trg_set_updated_at, 无需 DROP TRIGGER。
-- 索引随表级联删除, 无需显式 DROP INDEX。
-- 共用函数 trg_set_updated_at 由 000001 拥有, 本迁移不 DROP。
-- ============================================================

-- 孙表(依赖 strategy_items)
DROP TABLE IF EXISTS strategy_feedback;
DROP TABLE IF EXISTS strategy_attribution;

-- 子表(依赖 strategies)
DROP TABLE IF EXISTS strategy_items;

-- 父表
DROP TABLE IF EXISTS strategies;
