-- ============================================================
-- 000005_marketing.up.sql
-- 营销(券)域 (M2-S1)
-- 依据: 技术方案 第5章 §5.3.5 营销(券/拼团/老带新)域
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §10.1 裁剪):
--   纳入: coupons(券模板) + member_coupons(会员券实例)
--   裁剪: groupbuys / groupbuy_participants(拼团) 不建(依赖小程序码, 留 M2-S2);
--         wework_living_codes / community_groups / reach_tasks(社群/触达) 不建(留 M2-S2);
--         ai_strategies / strategy_items(AI 策略) 不建(留 M3-S1)。
--
-- 分区: ch05 无分区设计, 均普通表。
--
-- 其它约定(沿用 000001):
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at / updated_at 默认 now()。
--   * coupons 挂 BEFORE UPDATE 触发器刷新 updated_at + 软删除(deleted_at);
--     复用 000001 的 trg_set_updated_at() 函数, 此处仅 CREATE TRIGGER。
--   * member_coupons 为审计实例(强归因), 无 updated_at 不挂触发器, 禁软删(无 deleted_at)。
--   * 金额一律 BIGINT 分, 禁浮点; params 用 JSONB 存弹性参数。
-- ============================================================


-- ============================================================
-- 1. coupons : 券模板(软删除)
--    type: full_reduction(满减) | discount(折扣) | cash(代金) | gift(赠品)
--    status: draft(草稿) | active(上线) | paused(暂停) | ended(结束)
-- ============================================================
CREATE TABLE coupons (
    id           BIGSERIAL    PRIMARY KEY,
    uuid         UUID         NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id    BIGINT       NOT NULL REFERENCES tenants(id),
    store_id     BIGINT       REFERENCES stores(id),
    name         TEXT         NOT NULL,
    type         TEXT         NOT NULL
                 CHECK (type IN ('full_reduction','discount','cash','gift')),
    params       JSONB        NOT NULL DEFAULT '{}'::jsonb,
    valid_from   TIMESTAMPTZ  NOT NULL,
    valid_to     TIMESTAMPTZ  NOT NULL,
    total_quota  BIGINT,                                 -- 发行总量(分? 否, 张数); NULL=无限
    issued       BIGINT       NOT NULL DEFAULT 0,        -- 已发行数
    status       TEXT         NOT NULL DEFAULT 'draft'
                 CHECK (status IN ('draft','active','paused','ended')),
    created_by   BIGINT       REFERENCES users(id),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at   TIMESTAMPTZ,
    CHECK (valid_to > valid_from),
    CHECK (issued >= 0),
    CHECK (total_quota IS NULL OR total_quota >= 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_coupons_tenant_name ON coupons (tenant_id, name) WHERE deleted_at IS NULL;
CREATE INDEX idx_coupons_tenant_store ON coupons(tenant_id, store_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_coupons_tenant_status ON coupons(tenant_id, status) WHERE deleted_at IS NULL;

CREATE TRIGGER trg_coupons_updated_at
    BEFORE UPDATE ON coupons
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  coupons              IS '券模板; 按 tenant_id 行级隔离; 软删除; status 状态机 draft→active↔paused→ended';
COMMENT ON COLUMN coupons.id           IS '券模板自增主键';
COMMENT ON COLUMN coupons.uuid         IS '对外公开ID, 不暴露自增序号';
COMMENT ON COLUMN coupons.tenant_id    IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN coupons.store_id     IS '适用门店ID(外键 -> stores.id), NULL=全租户通用';
COMMENT ON COLUMN coupons.name         IS '券名称, 租户内唯一(未删除时)';
COMMENT ON COLUMN coupons.type         IS '券类型: full_reduction(满减) | discount(折扣) | cash(代金) | gift(赠品)';
COMMENT ON COLUMN coupons.params       IS '弹性参数 JSONB: {threshold_cents, off_cents, discount_permille, gift_product_id}';
COMMENT ON COLUMN coupons.valid_from   IS '生效时间(UTC)';
COMMENT ON COLUMN coupons.valid_to     IS '失效时间(UTC), 须 > valid_from';
COMMENT ON COLUMN coupons.total_quota  IS '发行总量(张), NULL=无限发行';
COMMENT ON COLUMN coupons.issued       IS '已发行数, <= total_quota(若 total_quota 非 NULL)';
COMMENT ON COLUMN coupons.status       IS '状态: draft(草稿) | active(上线) | paused(暂停) | ended(结束)';
COMMENT ON COLUMN coupons.created_by   IS '创建人ID(外键 -> users.id), 可空';
COMMENT ON COLUMN coupons.created_at   IS '创建时间(UTC)';
COMMENT ON COLUMN coupons.updated_at   IS '更新时间(UTC), 由触发器自动刷新';
COMMENT ON COLUMN coupons.deleted_at   IS '软删除时间, NULL 表示未删除';


-- ============================================================
-- 2. member_coupons : 会员券实例(强归因, 禁软删)
--    status: unused(未用) | used(已核销) | expired(过期) | revoked(撤回)
--    source: ai_strategy(AI发券) | manual(手动) | groupbuy(拼团) | newmember(新人)
--    strategy_id: AI 策略归因(效果回收关键链路), NULL=非AI来源
-- ============================================================
CREATE TABLE member_coupons (
    id           BIGSERIAL    PRIMARY KEY,
    tenant_id    BIGINT       NOT NULL REFERENCES tenants(id),
    member_id    BIGINT       NOT NULL REFERENCES members(id),
    coupon_id    BIGINT       NOT NULL REFERENCES coupons(id),
    status       TEXT         NOT NULL DEFAULT 'unused'
                 CHECK (status IN ('unused','used','expired','revoked')),
    source       TEXT         NOT NULL
                 CHECK (source IN ('ai_strategy','manual','groupbuy','newmember')),
    strategy_id  BIGINT,                                 -- AI 策略归因(外键 -> ai_strategy_items.id, M3 建表后补 FK)
    issued_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    used_at      TIMESTAMPTZ,                            -- 核销时间
    expire_at    TIMESTAMPTZ,                            -- 实例过期时间(NULL=用券模板 valid_to)
    ref_order_id BIGINT,                                 -- 核销关联订单
    UNIQUE (tenant_id, member_id, coupon_id, issued_at)
);

CREATE INDEX idx_mcoupon_member_status ON member_coupons(tenant_id, member_id, status) WHERE status = 'unused';
CREATE INDEX idx_mcoupon_strategy ON member_coupons(tenant_id, strategy_id) WHERE strategy_id IS NOT NULL;
CREATE INDEX idx_mcoupon_tenant_created ON member_coupons(tenant_id, issued_at);

COMMENT ON TABLE  member_coupons              IS '会员券实例; 强归因禁软删; source/strategy_id 是 AI 效果回收关键链路';
COMMENT ON COLUMN member_coupons.id           IS '实例自增主键';
COMMENT ON COLUMN member_coupons.tenant_id    IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN member_coupons.member_id    IS '会员ID(外键 -> members.id)';
COMMENT ON COLUMN member_coupons.coupon_id    IS '券模板ID(外键 -> coupons.id)';
COMMENT ON COLUMN member_coupons.status       IS '状态: unused(未用) | used(已核销) | expired(过期) | revoked(撤回)';
COMMENT ON COLUMN member_coupons.source       IS '来源: ai_strategy(AI发券) | manual(手动) | groupbuy(拼团) | newmember(新人)';
COMMENT ON COLUMN member_coupons.strategy_id  IS 'AI策略项ID(归因), NULL=非AI来源; M3 建表后补外键';
COMMENT ON COLUMN member_coupons.issued_at    IS '发券时间(UTC)';
COMMENT ON COLUMN member_coupons.used_at      IS '核销时间(UTC), unused 时为 NULL';
COMMENT ON COLUMN member_coupons.expire_at    IS '实例过期时间(UTC), NULL=沿用券模板 valid_to';
COMMENT ON COLUMN member_coupons.ref_order_id IS '核销关联订单ID, 可空';
