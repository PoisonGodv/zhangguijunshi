-- ============================================================
-- 000016_store_cside.up.sql
-- 门店 C 端可见字段扩展 (FE-MP-02 / FE-ADMIN, CONTRACT §16.11)
-- 依据: 技术方案 第5章 §5.3.1 用户/租户/门店域; CONTRACT §16.11
-- 目标数据库: PostgreSQL 16
--
-- 范围(纯加性, 不改 000001 既有列/约束/触发器):
--   给 stores 表补三列 C 端 消费者可见字段 ——
--     phone     TEXT  门店联系电话(可空; C 端"联系门店"用)
--     group_qr  TEXT  企微社群二维码图片URL/key(可空; 私域引流用)
--     active    BOOLEAN NOT NULL DEFAULT TRUE
--              (C 端列表只回 active=TRUE; 000001 的 status='active'/'closed'
--               仍保留供 boss/admin 用, 本列是 C 端口径的冗余活跃位,
--               默认 TRUE 与既有 status='active' 语义对齐)
--
-- 设计取舍(为什么不复用 status):
--   000001 的 stores.status 是 TEXT 枚举('active'/'closed'), 表达"门店营业状态",
--   供 boss/admin 门店管理用。C 端只需"是否对消费者可见"一个布尔, 用独立 active
--   列更显式(且 boss 可临时把 active=FALSE 隐藏某店而不改 status)。两者并存, 互不耦合。
--
-- 其它约定(沿用 000001):
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at 默认 now()。
--   * updated_at 由 000001 既有的 trg_stores_updated_at 触发器自动刷新, 本迁移不重建。
--   * 金额/坐标等 000001 既有列不动; 本迁移只加三列。
-- ============================================================

ALTER TABLE stores ADD COLUMN IF NOT EXISTS phone    TEXT;
ALTER TABLE stores ADD COLUMN IF NOT EXISTS group_qr TEXT;
ALTER TABLE stores ADD COLUMN IF NOT EXISTS active   BOOLEAN NOT NULL DEFAULT TRUE;

COMMENT ON COLUMN stores.phone    IS '门店联系电话(C 端"联系门店"用, 可空)';
COMMENT ON COLUMN stores.group_qr IS '企微社群二维码图片URL/key(私域引流用, 可空)';
COMMENT ON COLUMN stores.active   IS 'C 端可见活跃位(TRUE=对消费者可见); 与 status 并存, 互不耦合';
