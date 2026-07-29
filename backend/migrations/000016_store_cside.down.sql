-- ============================================================
-- 000016_store_cside.down.sql
-- 逆序回滚 000016: 删除 stores 的 phone / group_qr / active 三列
-- (纯加性回滚, 不影响 000001 既有列/约束/触发器)
-- ============================================================

ALTER TABLE stores DROP COLUMN IF EXISTS phone;
ALTER TABLE stores DROP COLUMN IF EXISTS group_qr;
ALTER TABLE stores DROP COLUMN IF EXISTS active;
