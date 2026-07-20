-- 000005_marketing.down.sql
-- 逆序 drop: 先 member_coupons(子) 后 coupons(父)
DROP TABLE IF EXISTS member_coupons;
DROP TABLE IF EXISTS coupons;
