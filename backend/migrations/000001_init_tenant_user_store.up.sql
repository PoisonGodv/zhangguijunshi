-- ============================================================
-- 000001_init_tenant_user_store.up.sql
-- 租户 / 用户 / 门店 域初始化 (M0-S1 地基)
-- 依据: 技术方案 第5章 §5.3.1 用户/租户/门店域
-- 目标数据库: PostgreSQL 16
--
-- 设计自洽说明 (tenant_id):
--   tenants 表本身不另设 tenant_id 列 —— 其主键 id 即 tenant_id,
--   作为所有业务表的租户行级隔离键(与 ch05 COMMENT 一致)。
--   业务表 users / stores 显式带 tenant_id BIGINT NOT NULL REFERENCES tenants(id)。
--
-- 其它约定:
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at / updated_at 默认 now();
--     updated_at 由 BEFORE UPDATE 触发器自动刷新(共用函数 trg_set_updated_at)。
--   * 软删除 deleted_at; 部分索引带 WHERE deleted_at IS NULL。
--   * 命名: 表复数 snake_case; 外键列 <singular>_id; 枚举用 TEXT + CHECK。
--   * gen_random_uuid() 在 PG13+ 已内置于核心(PG16 无需 CREATE EXTENSION)。
--   * golang-migrate 原生支持 $$ 美元引用字符串解析触发器函数, 无需额外标记。
-- ============================================================

-- ------------------------------------------------------------
-- 共用触发器函数: UPDATE 时自动刷新 updated_at
-- ------------------------------------------------------------
CREATE OR REPLACE FUNCTION trg_set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;


-- ============================================================
-- 1. tenants : 商户(租户)主体
--    id 即 tenant_id (全局租户隔离键)
-- ============================================================
CREATE TABLE tenants (
    id           BIGSERIAL    PRIMARY KEY,
    uuid         UUID         NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    name         TEXT         NOT NULL,
    license_no   TEXT,                                  -- 营业执照号
    license_type TEXT         NOT NULL DEFAULT 'individual'
                 CHECK (license_type IN ('individual','enterprise')),
    status       TEXT         NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','suspended','closed')),
    plan         TEXT         NOT NULL DEFAULT 'free'
                 CHECK (plan IN ('free','pro','flagship')),
    province     TEXT,
    city         TEXT,
    district     TEXT,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ
);

CREATE INDEX idx_tenants_status ON tenants(status) WHERE deleted_at IS NULL;

CREATE TRIGGER trg_tenants_updated_at
    BEFORE UPDATE ON tenants
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  tenants              IS '商户(租户)主体表; id 即 tenant_id, 作为所有业务表的租户行级隔离键';
COMMENT ON COLUMN tenants.id           IS 'tenant_id, 自增主键, 全局租户隔离键(其它业务表的 tenant_id 外键指向本列)';
COMMENT ON COLUMN tenants.uuid         IS '对外公开ID, 不暴露自增序号';
COMMENT ON COLUMN tenants.name         IS '商户名称';
COMMENT ON COLUMN tenants.license_no   IS '营业执照号';
COMMENT ON COLUMN tenants.license_type IS '执照类型: individual(个体) | enterprise(企业)';
COMMENT ON COLUMN tenants.status       IS '租户状态: active(正常) | suspended(暂停) | closed(关闭)';
COMMENT ON COLUMN tenants.plan         IS '订阅套餐: free | pro | flagship';
COMMENT ON COLUMN tenants.province     IS '省';
COMMENT ON COLUMN tenants.city         IS '市';
COMMENT ON COLUMN tenants.district     IS '区/县';
COMMENT ON COLUMN tenants.created_at   IS '创建时间(UTC)';
COMMENT ON COLUMN tenants.updated_at   IS '更新时间(UTC), 由触发器自动刷新';
COMMENT ON COLUMN tenants.deleted_at   IS '软删除时间, NULL 表示未删除';


-- ============================================================
-- 2. stores : 门店(一商户可多店)
-- ============================================================
CREATE TABLE stores (
    id             BIGSERIAL    PRIMARY KEY,
    uuid           UUID         NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id      BIGINT       NOT NULL REFERENCES tenants(id),
    name           TEXT         NOT NULL,
    business_type  TEXT         NOT NULL             -- fruit|maternity|convenience|clothing|fresh
                   CHECK (business_type IN ('fruit','maternity','convenience','clothing','fresh')),
    province       TEXT,
    city           TEXT,
    district       TEXT,
    address        TEXT,
    lng            DOUBLE PRECISION,
    lat            DOUBLE PRECISION,
    wework_corp_id TEXT,                              -- 企微企业ID
    status         TEXT         NOT NULL DEFAULT 'active'
                   CHECK (status IN ('active','closed')),
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at     TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_stores_tenant_name
    ON stores (tenant_id, name) WHERE deleted_at IS NULL;

CREATE INDEX idx_stores_tenant ON stores(tenant_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_stores_lbs    ON stores(tenant_id, lng, lat) WHERE deleted_at IS NULL;

CREATE TRIGGER trg_stores_updated_at
    BEFORE UPDATE ON stores
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  stores                IS '门店表; 一个商户(tenant)可拥有多家门店';
COMMENT ON COLUMN stores.id             IS '门店自增主键';
COMMENT ON COLUMN stores.uuid           IS '对外公开ID, 不暴露自增序号';
COMMENT ON COLUMN stores.tenant_id      IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN stores.name           IS '门店名称';
COMMENT ON COLUMN stores.business_type  IS '业态: fruit(水果) | maternity(母婴) | convenience(便利店) | clothing(服装) | fresh(生鲜)';
COMMENT ON COLUMN stores.lng            IS '门店经度(WGS84)';
COMMENT ON COLUMN stores.lat            IS '门店纬度(WGS84)';
COMMENT ON COLUMN stores.wework_corp_id IS '企业微信企业ID';
COMMENT ON COLUMN stores.status         IS '门店状态: active(营业) | closed(关闭)';
COMMENT ON COLUMN stores.created_at     IS '创建时间(UTC)';
COMMENT ON COLUMN stores.updated_at     IS '更新时间(UTC), 由触发器自动刷新';
COMMENT ON COLUMN stores.deleted_at     IS '软删除时间, NULL 表示未删除';


-- ============================================================
-- 3. users : 员工/账号(店主、店长、店员、收银、区域经理、平台运营)
-- ============================================================
CREATE TABLE users (
    id            BIGSERIAL    PRIMARY KEY,
    uuid          UUID         NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id     BIGINT       NOT NULL REFERENCES tenants(id),
    store_id      BIGINT       REFERENCES stores(id),           -- 可空(区域经理/平台运营)
    name          TEXT         NOT NULL,
    phone_enc     BYTEA,                                         -- 手机号字段级加密密文
    phone_hash    TEXT,                                          -- SHA256+盐, 用于等值查询
    wx_openid     TEXT,                                          -- 商家小程序 openid
    wx_unionid    TEXT,                                          -- 微信 unionid
    role          TEXT         NOT NULL DEFAULT 'clerk'
                  CHECK (role IN ('owner','manager','clerk','cashier','regional_mgr','platform_ops')),
    status        TEXT         NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','disabled')),
    last_login_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tenant_phone_hash
    ON users (tenant_id, phone_hash) WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_users_tenant_wx_openid
    ON users (tenant_id, wx_openid) WHERE deleted_at IS NULL AND wx_openid IS NOT NULL;

CREATE INDEX idx_users_tenant_role ON users(tenant_id, role) WHERE deleted_at IS NULL;

CREATE TRIGGER trg_users_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  users               IS '员工/账号表(店主、店长、店员、收银、区域经理、平台运营)';
COMMENT ON COLUMN users.id            IS '用户自增主键';
COMMENT ON COLUMN users.uuid          IS '对外公开ID, 不暴露自增序号';
COMMENT ON COLUMN users.tenant_id     IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN users.store_id      IS '所属门店ID(外键 -> stores.id), 可空(如区域经理/平台运营)';
COMMENT ON COLUMN users.name          IS '员工姓名';
COMMENT ON COLUMN users.phone_enc     IS '手机号字段级加密密文(BYTEA)';
COMMENT ON COLUMN users.phone_hash    IS '手机号 SHA256+盐, 用于等值查询与唯一约束';
COMMENT ON COLUMN users.wx_openid     IS '商家小程序 openid';
COMMENT ON COLUMN users.wx_unionid    IS '微信 unionid';
COMMENT ON COLUMN users.role          IS '角色: owner(店主) | manager(店长) | clerk(店员) | cashier(收银) | regional_mgr(区域经理) | platform_ops(平台运营)';
COMMENT ON COLUMN users.status        IS '账号状态: active(启用) | disabled(禁用)';
COMMENT ON COLUMN users.last_login_at IS '最近登录时间(UTC)';
COMMENT ON COLUMN users.created_at    IS '创建时间(UTC)';
COMMENT ON COLUMN users.updated_at    IS '更新时间(UTC), 由触发器自动刷新';
COMMENT ON COLUMN users.deleted_at    IS '软删除时间, NULL 表示未删除';
