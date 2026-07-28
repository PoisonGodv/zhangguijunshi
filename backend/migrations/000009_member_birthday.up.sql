-- ============================================================
-- 000009_member_birthday.up.sql
-- M3-S2: members 表加 birthday 列 (signal 真实枚举当日生日会员用)
-- 依据: CONTRACT §13.2 / §13.3.1
--
-- 加性扩展, 不改现有列/约束/触发器。last_visit_at 已在 000004 建立,
-- 本迁移只补 birthday (CONTRACT §13.3.1: "A1 自查现有 Member struct +
-- migration 000004, 按需补字段" —— last_visit_at 已存在, 仅加 birthday)。
-- ============================================================

ALTER TABLE members ADD COLUMN birthday DATE;

COMMENT ON COLUMN members.birthday IS '会员生日 (YYYY-MM-DD), 可空, M3-S2 signal 当日生日枚举用';
