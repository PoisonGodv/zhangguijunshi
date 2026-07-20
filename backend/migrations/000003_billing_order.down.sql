-- ============================================================
-- 000003_billing_order.down.sql
-- 回滚 000003: 逆序删除 order_items -> orders
-- 依赖关系: order_items.order_id -> orders.id,
--           order_items.product_id -> products.id (000002, 本迁移不动);
--   故删表顺序必须先子表(order_items)后父表(orders)。
-- 删表会级联删除挂在其上的 BEFORE UPDATE 触发器; 共用函数
--   trg_set_updated_at 由 000001 拥有, 本迁移不 DROP。
-- ============================================================

DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
