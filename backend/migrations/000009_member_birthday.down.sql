-- ============================================================
-- 000009_member_birthday.down.sql
-- 逆序回滚 000009: 删除 members.birthday 列
-- ============================================================

ALTER TABLE members DROP COLUMN IF EXISTS birthday;
