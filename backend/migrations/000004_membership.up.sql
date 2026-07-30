-- ============================================================
-- 000004_membership.up.sql
-- 会员 / 储值域 (M1-S2)
-- 依据: 技术方案 第5章 §5.3.4 会员/储值域
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §9.1 裁剪):
--   纳入: members + member_balance_accounts + member_balance_log + member_points_log
--   裁剪: coupons / member_coupons(券核销) 不建(营销域 M2),
--         payment_ledger / sub_merchants(真实资金/反二清) 不建(M4),
--         留待后续迁移。
--
-- 分区降级(M1-S2 决策, 见 CONTRACT §9.1/§9.2):
--   ch05 中 member_balance_log 设计为 PARTITION BY RANGE (created_at),
--   主键写为 PRIMARY KEY (id, created_at)。本里程碑为可迁移性建【普通表】
--   (不带 PARTITION BY), 改用普通 BIGSERIAL PRIMARY KEY (id);
--   月分区滚动留待后续迁移(线上数据量未到分区阈值前普通表即够)。
--
-- 其它约定(沿用 000001):
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at / updated_at 默认 now()。
--   * members / member_balance_accounts 挂 BEFORE UPDATE 触发器刷新 updated_at
--     (共用函数 trg_set_updated_at 已在 000001 创建, 此处仅 CREATE TRIGGER)。
--   * member_balance_log / member_points_log 为强审计流水, 无 updated_at
--     且不挂触发器, 禁软删(无 deleted_at 列)。
--   * 金额一律 BIGINT 分, 禁浮点。
--   * inventory_batches / inventory_movements 已在 000002 建好, 本次不重复建。
-- ============================================================


-- ============================================================
-- 1. members : 会员主档(软删除)
-- ============================================================
CREATE TABLE members (
    id                BIGSERIAL    PRIMARY KEY,
    uuid              UUID         NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id         BIGINT       NOT NULL REFERENCES tenants(id),
    store_id          BIGINT       NOT NULL REFERENCES stores(id),
    phone_enc         BYTEA,                                 -- 手机号密文(可选, 加密存储)
    phone_hash        TEXT,                                  -- 手机号哈希(检索/幂等键)
    wx_openid         TEXT,                                  -- C端小程序 openid
    wx_unionid        TEXT,                                  -- 微信 unionid(跨应用)
    name              TEXT,                                  -- 会员昵称/姓名
    level             TEXT         NOT NULL DEFAULT 'normal'
                      CHECK (level IN ('normal','vip','blackcard')),
    points            BIGINT       NOT NULL DEFAULT 0,       -- 当前积分余额
    total_spent_cents BIGINT       NOT NULL DEFAULT 0,       -- 近90天累计消费(分, 升降级用)
    last_visit_at     TIMESTAMPTZ,                           -- 最近到访/消费时间
    status            TEXT         NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active','blocked')),
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at        TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_members_tenant_phone ON members (tenant_id, phone_hash) WHERE deleted_at IS NULL AND phone_hash IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_members_tenant_openid ON members (tenant_id, wx_openid) WHERE deleted_at IS NULL AND wx_openid IS NOT NULL;

CREATE INDEX idx_members_tenant_store ON members(tenant_id, store_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_members_last_visit ON members(tenant_id, store_id, last_visit_at) WHERE deleted_at IS NULL;

CREATE TRIGGER trg_members_updated_at
    BEFORE UPDATE ON members
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  members                   IS '会员主档; 按 tenant_id + store_id 行级隔离; 软删除';
COMMENT ON COLUMN members.id                IS '会员自增主键';
COMMENT ON COLUMN members.uuid              IS '对外公开ID, 不暴露自增序号';
COMMENT ON COLUMN members.tenant_id         IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN members.store_id          IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN members.phone_enc         IS '手机号密文(加密存储, 可选)';
COMMENT ON COLUMN members.phone_hash        IS '手机号哈希, 检索/幂等键, 租户内唯一(可空)';
COMMENT ON COLUMN members.wx_openid         IS 'C端小程序 openid, 租户内唯一(可空)';
COMMENT ON COLUMN members.wx_unionid        IS '微信 unionid(跨应用)';
COMMENT ON COLUMN members.name              IS '会员昵称/姓名';
COMMENT ON COLUMN members.level             IS '会员等级: normal(普通) | vip | blackcard(黑卡)';
COMMENT ON COLUMN members.points            IS '当前积分余额';
COMMENT ON COLUMN members.total_spent_cents IS '近90天累计消费(分), 升降级判定用';
COMMENT ON COLUMN members.last_visit_at     IS '最近到访/消费时间(UTC), 可空';
COMMENT ON COLUMN members.status            IS '会员状态: active(正常) | blocked(冻结)';
COMMENT ON COLUMN members.created_at        IS '创建时间(UTC)';
COMMENT ON COLUMN members.updated_at        IS '更新时间(UTC), 由触发器自动刷新';
COMMENT ON COLUMN members.deleted_at        IS '软删除时间, NULL 表示未删除';


-- ============================================================
-- 2. member_balance_accounts : 储值账户(每会员一户)
--    反二清: 余额仅记账, 资金不沉淀平台。
-- ============================================================
CREATE TABLE member_balance_accounts (
    id               BIGSERIAL    PRIMARY KEY,
    tenant_id        BIGINT       NOT NULL REFERENCES tenants(id),
    member_id        BIGINT       NOT NULL REFERENCES members(id),
    balance_cents    BIGINT       NOT NULL DEFAULT 0,        -- 账面余额(分)
    gift_cents       BIGINT       NOT NULL DEFAULT 0,        -- 赠送部分(分, 退款规则区分)
    region_cap_cents BIGINT,                                 -- 地区单卡余额上限(分, 上海/江苏)
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, member_id),
    CHECK (balance_cents >= 0)
);

CREATE INDEX idx_member_balance_tenant_member ON member_balance_accounts(tenant_id, member_id);

CREATE TRIGGER trg_member_balance_accounts_updated_at
    BEFORE UPDATE ON member_balance_accounts
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  member_balance_accounts                  IS '会员储值账户; 每会员一户; 余额仅记账(反二清)';
COMMENT ON COLUMN member_balance_accounts.id               IS '账户自增主键';
COMMENT ON COLUMN member_balance_accounts.tenant_id        IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN member_balance_accounts.member_id        IS '会员ID(外键 -> members.id)';
COMMENT ON COLUMN member_balance_accounts.balance_cents    IS '账面余额(分), 非负';
COMMENT ON COLUMN member_balance_accounts.gift_cents       IS '赠送余额(分), 退款规则区分';
COMMENT ON COLUMN member_balance_accounts.region_cap_cents IS '地区单卡余额上限(分, 上海/江苏), 可空';
COMMENT ON COLUMN member_balance_accounts.created_at       IS '创建时间(UTC)';
COMMENT ON COLUMN member_balance_accounts.updated_at       IS '更新时间(UTC), 由触发器自动刷新';


-- ============================================================
-- 3. member_balance_log : 储值流水(强审计, 禁软删)
--    M1-S2 降级为普通表(非分区); 无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE member_balance_log (
    id            BIGSERIAL    PRIMARY KEY,
    tenant_id     BIGINT       NOT NULL REFERENCES tenants(id),
    member_id     BIGINT       NOT NULL REFERENCES members(id),
    account_id    BIGINT       NOT NULL REFERENCES member_balance_accounts(id),
    type          TEXT         NOT NULL
                  CHECK (type IN ('recharge','consume','refund','adjust','gift_in')),
    amount_cents  BIGINT       NOT NULL,                     -- 金额(分), 正入负出
    balance_after BIGINT       NOT NULL,                     -- 流水后余额(分)
    ref_order_id  BIGINT,                                    -- 关联订单(消费/退款)
    operator_id   BIGINT       REFERENCES users(id),
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_member_balance_log_tenant_member ON member_balance_log(tenant_id, member_id, created_at);
CREATE INDEX idx_member_balance_log_tenant_created ON member_balance_log(tenant_id, created_at);

COMMENT ON TABLE  member_balance_log               IS '储值流水表, 强审计禁软删; M1-S2 普通表, 月分区滚动留待后续迁移';
COMMENT ON COLUMN member_balance_log.id            IS '流水自增主键';
COMMENT ON COLUMN member_balance_log.tenant_id     IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN member_balance_log.member_id     IS '会员ID(外键 -> members.id)';
COMMENT ON COLUMN member_balance_log.account_id    IS '储值账户ID(外键 -> member_balance_accounts.id)';
COMMENT ON COLUMN member_balance_log.type          IS '流水类型: recharge(充值) | consume(消费) | refund(退款) | adjust(调整) | gift_in(赠送入账)';
COMMENT ON COLUMN member_balance_log.amount_cents  IS '金额(分), 正入负出';
COMMENT ON COLUMN member_balance_log.balance_after IS '流水后余额(分)';
COMMENT ON COLUMN member_balance_log.ref_order_id  IS '关联订单ID(消费/退款), 可空';
COMMENT ON COLUMN member_balance_log.operator_id   IS '操作人ID(外键 -> users.id), 可空';
COMMENT ON COLUMN member_balance_log.created_at    IS '创建时间(UTC)';


-- ============================================================
-- 4. member_points_log : 积分流水(强审计, 禁软删)
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE member_points_log (
    id           BIGSERIAL    PRIMARY KEY,
    tenant_id    BIGINT       NOT NULL REFERENCES tenants(id),
    member_id    BIGINT       NOT NULL REFERENCES members(id),
    type         TEXT         NOT NULL
                 CHECK (type IN ('earn','redeem','expire','adjust')),
    delta        BIGINT       NOT NULL,                      -- 积分变动, 正加负减
    points_after BIGINT       NOT NULL,                      -- 变动后积分余额
    ref_order_id BIGINT,                                     -- 关联订单(获分/抵现)
    expire_date  DATE,                                       -- 该批积分到期日
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_points_tenant_member ON member_points_log(tenant_id, member_id, created_at);

COMMENT ON TABLE  member_points_log              IS '积分流水表, 强审计禁软删';
COMMENT ON COLUMN member_points_log.id           IS '流水自增主键';
COMMENT ON COLUMN member_points_log.tenant_id    IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN member_points_log.member_id    IS '会员ID(外键 -> members.id)';
COMMENT ON COLUMN member_points_log.type         IS '流水类型: earn(获得) | redeem(抵现/兑换) | expire(过期) | adjust(调整)';
COMMENT ON COLUMN member_points_log.delta        IS '积分变动, 正加负减';
COMMENT ON COLUMN member_points_log.points_after IS '变动后积分余额';
COMMENT ON COLUMN member_points_log.ref_order_id IS '关联订单ID(获分/抵现), 可空';
COMMENT ON COLUMN member_points_log.expire_date  IS '该批积分到期日, 可空';
COMMENT ON COLUMN member_points_log.created_at   IS '创建时间(UTC)';
