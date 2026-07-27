-- ============================================================
-- 000006_reach_community_groupbuy.down.sql
-- 回滚 000006: 逆序删除触达/社群/拼团域 7 张表
--
-- 依赖关系(FK):
--   groupbuy_participants.groupbuy_id -> groupbuys.id  (子->父, 必须先删 participants)
--   groupbuy_participants.member_id   -> members.id   (000004, 不在本迁移)
--   其余表(groupbuys/wework_living_codes/community_groups/reach_tasks/
--           subscribe_auth/unsubscribe_records)仅依赖 000001-000004 已建基础表
--           (tenants/stores/products/members), 彼此独立。
--
-- 删表顺序(先子后父):
--   独立表(任意序) -> groupbuy_participants(子) -> groupbuys(父)
--
-- 触发器:
--   groupbuys / community_groups 挂有 trg_<表>_updated_at; PG 中删表会
--   级联删除挂在其上的触发器, 但此处显式 DROP TRIGGER IF EXISTS 以与
--   up.sql 对称、自文档化(reach_tasks/subscribe_auth/unsubscribe_records/
--   wework_living_codes 无触发器, 无需 DROP)。
--
-- 共用函数 trg_set_updated_at 由 000001 拥有, 本迁移不 DROP。
-- 索引随表级联删除, 无需显式 DROP INDEX。
-- ============================================================

-- 独立表(彼此无 FK 依赖, 逆序清理)
DROP TABLE IF EXISTS unsubscribe_records;
DROP TABLE IF EXISTS subscribe_auth;
DROP TABLE IF EXISTS reach_tasks;
DROP TRIGGER IF EXISTS trg_community_groups_updated_at ON community_groups;
DROP TABLE IF EXISTS community_groups;
DROP TABLE IF EXISTS wework_living_codes;

-- 拼团: 先删 participants(子) 再删 groupbuys(父)
DROP TABLE IF EXISTS groupbuy_participants;
DROP TRIGGER IF EXISTS trg_groupbuys_updated_at ON groupbuys;
DROP TABLE IF EXISTS groupbuys;
