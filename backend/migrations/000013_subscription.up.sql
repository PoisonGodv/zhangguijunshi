-- ============================================================
-- 000013_subscription.up.sql
-- 订阅状态与流水 (M4-S1, BE-B-13 订阅付费墙)
-- 依据: 技术方案 第5章 + CONTRACT §15.2 / §15.3.2
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §15.1 裁剪):
--   纳入: subscriptions (订阅流水 + 当前态, 三层蛋糕 free/pro/flagship)。
--   裁剪: 自动续费 / 到期降级 cron 延后(§15.8, 本期 ConfirmPayment 激活即 active);
--         真实 PG 接入本期 Memory 双实现, 生产配 PG。
--
-- 设计要点:
--   * tenants.plan(000001)是冗余快照; 本表是权威流水(每次切换/续期一条记录)。
--   * UNIQUE 部分索引 (tenant_id WHERE status='active') 保证单租户至多一条
--     有效订阅; 切换 plan 时旧 active 记录由应用层置 cancelled。
--   * status: active(有效) | expired(过期, cron 置) | cancelled(已取消/切换)。
--   * source: manual | wxpay | alipay | admin (激活来源)。
--   * period_ends_at NULL = 永久(仅 free); 付费 plan 按月。
--   * 挂 BEFORE UPDATE 触发器刷新 updated_at (复用 000001 的 trg_set_updated_at())。
-- ============================================================

CREATE TABLE subscriptions (
    id                BIGSERIAL    PRIMARY KEY,
    tenant_id         BIGINT       NOT NULL REFERENCES tenants(id),
    plan              TEXT         NOT NULL                        -- free|pro|flagship
                      CHECK (plan IN ('free','pro','flagship')),
    period_started_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
    period_ends_at    TIMESTAMPTZ,                                  -- NULL=永久(仅 free)
    status            TEXT         NOT NULL DEFAULT 'active'       -- active|expired|cancelled
                      CHECK (status IN ('active','expired','cancelled')),
    source            TEXT         NOT NULL DEFAULT 'manual'       -- manual|wxpay|alipay|admin
                      CHECK (source IN ('manual','wxpay','alipay','admin')),
    payment_ref       TEXT,                                         -- 关联支付单 out_trade_no(可选)
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_subscriptions_tenant ON subscriptions(tenant_id, status);

-- 每租户至多一条 active 订阅(切换 plan 时旧记录置 cancelled)
CREATE UNIQUE INDEX uniq_subscriptions_active_one ON subscriptions(tenant_id) WHERE status='active';

CREATE TRIGGER trg_subscriptions_updated_at
    BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  subscriptions                       IS '订阅流水(M4-S1); 三层蛋糕 L1 free/L2 pro 39元月/L3 flagship; UNIQUE active 保证单租户单有效订阅';
COMMENT ON COLUMN subscriptions.id                    IS '订阅记录自增主键';
COMMENT ON COLUMN subscriptions.tenant_id             IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN subscriptions.plan                  IS '套餐: free | pro | flagship (与 tenants.plan CHECK 一致)';
COMMENT ON COLUMN subscriptions.period_started_at     IS '当前周期开始时间(UTC)';
COMMENT ON COLUMN subscriptions.period_ends_at        IS '当前周期结束时间(UTC), NULL=永久(仅 free)';
COMMENT ON COLUMN subscriptions.status                IS '状态: active(有效) | expired(过期,cron置) | cancelled(已取消/切换)';
COMMENT ON COLUMN subscriptions.source                IS '激活来源: manual | wxpay | alipay | admin';
COMMENT ON COLUMN subscriptions.payment_ref           IS '关联支付单 out_trade_no(可选, 支付回调激活时填)';
COMMENT ON COLUMN subscriptions.created_at            IS '创建时间(UTC)';
COMMENT ON COLUMN subscriptions.updated_at            IS '更新时间(UTC), 由触发器自动刷新';
