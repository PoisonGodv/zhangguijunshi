-- ============================================================
-- 000002_catalog_inventory.down.sql
-- 回滚 000002: 逆序删除 inventory_movements -> inventory_batches -> products
-- 依赖关系: inventory_movements.batch_id -> inventory_batches.id,
--           inventory_movements.product_id -> products.id,
--           inventory_batches.product_id  -> products.id;
--   故删表顺序必须先子表(inventory_movements)后父表(products)。
-- 删表会级联删除挂在其上的 BEFORE UPDATE 触发器; 共用函数
--   trg_set_updated_at 由 000001 拥有, 本迁移不 DROP。
-- ============================================================

DROP TABLE IF EXISTS inventory_movements;
DROP TABLE IF EXISTS inventory_batches;
DROP TABLE IF EXISTS products;
