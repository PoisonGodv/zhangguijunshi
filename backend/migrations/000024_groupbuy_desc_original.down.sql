-- ============================================================
-- 000024_groupbuy_desc_original.down.sql
-- 逆序回滚 000024_groupbuy_desc_original.up.sql
-- 幂等: 均 IF EXISTS。
-- ============================================================

ALTER TABLE groupbuys DROP COLUMN IF EXISTS original_price_cents;

ALTER TABLE groupbuys DROP COLUMN IF EXISTS description;
