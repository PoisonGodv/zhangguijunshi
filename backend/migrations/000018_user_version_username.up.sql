-- ============================================================
-- 000018_user_version_username.up.sql
-- users 域补列: token 失效版本号 version + 唯一登录名 username
-- 依据: CONTRACT §19 (BE-A-01 真登录/RBAC) 收尾
-- 目标数据库: PostgreSQL 16
--
-- 背景:
--   * version: 000001 建表时 users 无 version 列, PG repo 只能 SELECT 常量
--     0 (0 AS version), JWT 的 ver 校验恒为 no-op。这里补真实列, 为后续
--     "改密码即失效旧 token" (revoke-on-password-change) 提供钩子;
--     BumpVersion 在改密时 version+1, 旧 token 的 ver 不再匹配即被拒。
--   * username: 000001 只有 name(员工姓名, 全局不唯一), admin 登录被迫复用
--     name 列当登录名。这里补独立的 username 列, 并在 (tenant_id, username)
--     上建部分唯一索引(排除软删除与 NULL), 使平台运营账号登录名租户内唯一。
--
-- 约定:
--   * 唯一约束用 CREATE UNIQUE INDEX ... WHERE (不用内联 UNIQUE(...)WHERE,
--     golang-migrate 内联写法非法), 与 000001 idx_users_tenant_phone_hash 一致。
--   * 两列均允许存量行平滑兼容: version NOT NULL DEFAULT 0 回填; username 可空。
-- ============================================================

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 0;

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS username TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tenant_username
    ON users (tenant_id, username)
    WHERE deleted_at IS NULL AND username IS NOT NULL;

COMMENT ON COLUMN users.version  IS 'token 失效版本号, 默认 0; 改密时 +1 使旧 token 的 ver 失配(revoke-on-password-change)';
COMMENT ON COLUMN users.username IS '登录名(平台运营等), 租户内唯一(部分唯一索引排除软删除/NULL); 区别于员工姓名 name';
