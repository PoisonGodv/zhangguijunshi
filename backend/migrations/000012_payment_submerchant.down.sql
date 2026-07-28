-- ============================================================
-- 000012_payment_submerchant.down.sql
-- 逆序回滚 000012_payment_submerchant.up.sql (CONTRACT §15.2).
-- 先子后父: payment_ledger → payments → sub_merchants.
-- 触发器随表 DROP 自动清除(无需单独 DROP TRIGGER).
-- ============================================================

DROP TABLE IF EXISTS payment_ledger;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS sub_merchants;
