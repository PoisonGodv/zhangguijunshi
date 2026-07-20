-- ============================================================
-- 000004_membership.down.sql
-- 回滚 000004: 逆序删除 member_points_log -> member_balance_log
--   -> member_balance_accounts -> members
-- 依赖关系: member_balance_log.member_id/account_id ->
--             members.id / member_balance_accounts.id,
--           member_points_log.member_id -> members.id,
--           member_balance_accounts.member_id -> members.id;
--   故删表顺序必须先子表(log/points_log/account)后父表(members)。
-- 删表会级联删除挂在其上的 BEFORE UPDATE 触发器; 共用函数
--   trg_set_updated_at 由 000001 拥有, 本迁移不 DROP。
-- ============================================================

DROP TABLE IF EXISTS member_points_log;
DROP TABLE IF EXISTS member_balance_log;
DROP TABLE IF EXISTS member_balance_accounts;
DROP TABLE IF EXISTS members;
