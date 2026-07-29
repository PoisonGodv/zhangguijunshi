-- 000014_wx_sessions.up.sql
-- auth 域 wx_sessions 表 (CONTRACT §17.7, M4-S2 pgx 全局替换)。
-- 此前 auth.ConsumerRepo 只有 memory 实现; 本迁移为其补齐持久化层。
-- 表结构对齐 auth.Consumer: (tenant_id, openid) 唯一, session_key 服务端缓存,
-- version 为 token 版本号, member_id 留 0 (membership.Join 跨域回填属另一期, §17.8)。
-- 租户行级隔离: 每条读写 WHERE 带 tenant_id (ARCH-01)。
CREATE TABLE wx_sessions (
  id           BIGSERIAL PRIMARY KEY,
  tenant_id    BIGINT      NOT NULL,
  openid       TEXT        NOT NULL,
  unionid      TEXT        NOT NULL DEFAULT '',
  member_id    BIGINT      NOT NULL DEFAULT 0,
  session_key  TEXT        NOT NULL DEFAULT '',   -- SERVER-SIDE ONLY
  version      BIGINT      NOT NULL DEFAULT 0,    -- token version
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, openid)
);
CREATE INDEX idx_wx_sessions_tenant_openid ON wx_sessions (tenant_id, openid);
