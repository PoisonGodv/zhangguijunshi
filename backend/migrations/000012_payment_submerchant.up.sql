-- ============================================================
-- 000012_payment_submerchant.up.sql
-- 微信支付服务商进件 + 支付单 + 支付流水(M4-S1, WX-07 / BE-A-10).
-- 依据: 技术方案 第5章 §5.3.3 / §5.3.7 + CONTRACT §15.2 (M4-S1 迁移编号 DB-06).
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §15.1):
--   * sub_merchants : 服务商模式批量进件的特约商户(子商户)记录. 进件成功后
--     sub_mch_id 为特约商户号, 是 payments 的清算方(反二清: 资金由微信直清
--     特约商户, 不沉淀平台).
--   * payments      : 业务侧支付单, 状态机 pending→success(不可逆). subject 是
--     履约分发键(order|recharge|subscription).
--   * payment_ledger: 支付流水(反二清审计, 禁软删). UNIQUE(tenant_id, provider,
--     provider_tx_id) 是回调幂等核心(同一微信交易号全局唯一).
--
-- 反二清命门(CONTRACT §15.5.3): 本表族不持有资金, payment_ledger 仅审计记账,
-- payments.sub_merchant_id 标记清算方. 平台无余额沉淀概念.
--
-- 其它约定(沿用 000001-000011):
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at/updated_at 默认 now().
--   * tenant_id NOT NULL REFERENCES tenants(id), 行级隔离键; store_id REFERENCES stores(id).
--   * 金额一律 BIGINT 分.
--   * updated_at 由 BEFORE UPDATE 触发器自动刷新(共用函数 trg_set_updated_at).
--   * payment_ledger 无 updated_at → 不挂触发器(纯审计流水).
--   * 本期不分区: ch05 §5.3.3 原设计 payment_ledger 按月分区(PARTITION BY RANGE
--     created_at) + UNIQUE 含 created_at, 本期单表 + 简化 UNIQUE, 分区延后生产
--     (CONTRACT §15.1 裁剪 / §15.8).
-- ============================================================


-- ------------------------------------------------------------
-- 1. sub_merchants : 特约商户(子商户)进件记录
--    sub_mch_id NULL = 未进件成功(pending/reviewing/rejected/frozen).
--    uniq_submch_provider_mchid: 同租户同 provider 同 sub_mch_id 仅一条(进件成功后).
-- ------------------------------------------------------------
CREATE TABLE sub_merchants (
    id           BIGSERIAL    PRIMARY KEY,
    tenant_id    BIGINT       NOT NULL REFERENCES tenants(id),
    store_id     BIGINT       NOT NULL REFERENCES stores(id),
    provider     TEXT         NOT NULL,                         -- wxpay|alipay
    sub_mch_id   TEXT,                                          -- 进件成功后的特约商户号
    applyment_id TEXT,                                          -- 微信申请单号
    status       TEXT         NOT NULL DEFAULT 'pending'        -- pending|reviewing|active|rejected|frozen
                 CHECK (status IN ('pending','reviewing','active','rejected','frozen')),
    rate_bps     INT,                                            -- 费率基点 38=0.38%
    materials    JSONB,                                          -- 进件材料快照(脱敏)
    applied_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    approved_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uniq_submch_provider_mchid ON sub_merchants(tenant_id, provider, sub_mch_id) WHERE sub_mch_id IS NOT NULL;
CREATE INDEX idx_submch_tenant_status ON sub_merchants(tenant_id, status);
COMMENT ON TABLE  sub_merchants IS '特约商户进件(M4-S1); 服务商模式批量进件; sub_mch_id NULL=未进件成功; 反二清: payments.sub_merchant_id 指向本表清算方';
COMMENT ON COLUMN sub_merchants.sub_mch_id IS '进件成功后的特约商户号(空=未成功); 资金由微信直清该特约商户, 平台不碰资金';
COMMENT ON COLUMN sub_merchants.status     IS 'pending|reviewing|active|rejected|frozen';
COMMENT ON COLUMN sub_merchants.rate_bps   IS '费率基点 38=0.38%';
COMMENT ON COLUMN sub_merchants.materials  IS '进件材料快照(脱敏, JSONB)';


-- ------------------------------------------------------------
-- 2. payments : 业务侧支付单(状态机 pending→success)
--    sub_merchant_id 标记清算方(反二清). out_trade_no 商户订单号(UNIQUE per tenant).
--    subject 履约分发键: order→billing.MarkOrderPaid, recharge→membership.Recharge,
--    subscription→subscription.ConfirmPayment.
-- ------------------------------------------------------------
CREATE TABLE payments (
    id              BIGSERIAL    PRIMARY KEY,
    tenant_id       BIGINT       NOT NULL REFERENCES tenants(id),
    store_id        BIGINT       NOT NULL REFERENCES stores(id),
    sub_merchant_id BIGINT       NOT NULL,                        -- 清算到的特约商户(反二清)
    out_trade_no    TEXT         NOT NULL,                        -- 商户订单号(业务侧生成, UNIQUE)
    provider        TEXT         NOT NULL,                        -- wxpay|alipay
    provider_tx_id  TEXT,                                          -- 微信/支付宝交易号(回调填, 对账键)
    subject         TEXT         NOT NULL,                        -- order|recharge|subscription (履约分发)
    ref_id          BIGINT,                                        -- 按 subject 解释: orderID/memberID/0(subscription)
    amount_cents    BIGINT       NOT NULL,
    type            TEXT         NOT NULL DEFAULT 'pay'           -- pay|refund
                    CHECK (type IN ('pay','refund')),
    status          TEXT         NOT NULL DEFAULT 'pending'       -- pending|success|failed|refunded
                    CHECK (status IN ('pending','success','failed','refunded')),
    paid_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, out_trade_no)
);
CREATE INDEX idx_payments_tenant_status ON payments(tenant_id, status);
CREATE INDEX idx_payments_txid          ON payments(tenant_id, provider, provider_tx_id) WHERE provider_tx_id IS NOT NULL;
COMMENT ON TABLE  payments            IS '支付单(M4-S1); 状态机 pending→success 不可逆; 反二清: sub_merchant_id 标记清算方, 资金由微信直清特约商户';
COMMENT ON COLUMN payments.subject         IS '履约分发键: order→billing.MarkOrderPaid, recharge→membership.Recharge, subscription→subscription.ConfirmPayment';
COMMENT ON COLUMN payments.out_trade_no     IS '商户订单号(业务侧生成), UNIQUE(tenant_id, out_trade_no)';
COMMENT ON COLUMN payments.provider_tx_id   IS '微信/支付宝交易号(回调填, 对账键)';
COMMENT ON COLUMN payments.sub_merchant_id  IS '清算到的特约商户(反二清); 指向 sub_merchants.id';


-- ------------------------------------------------------------
-- 3. payment_ledger : 支付流水(反二清审计; 禁软删)
--    UNIQUE(tenant_id, provider, provider_tx_id) = 回调幂等核心(同一交易号全局唯一).
--    无 updated_at → 不挂 trg_set_updated_at(纯审计流水).
-- ------------------------------------------------------------
CREATE TABLE payment_ledger (
    id              BIGSERIAL    PRIMARY KEY,
    tenant_id       BIGINT       NOT NULL REFERENCES tenants(id),
    store_id        BIGINT       NOT NULL REFERENCES stores(id),
    sub_merchant_id BIGINT       NOT NULL,
    order_id        BIGINT,                                        -- 可 0(非订单支付)
    provider        TEXT         NOT NULL,
    provider_tx_id  TEXT,                                          -- UNIQUE(tenant,provider,tx_id) 幂等核心
    amount_cents    BIGINT       NOT NULL,
    fee_cents       BIGINT       NOT NULL DEFAULT 0,               -- 手续费
    type            TEXT         NOT NULL CHECK (type IN ('pay','refund')),
    status          TEXT         NOT NULL CHECK (status IN ('success','pending','failed','refunded')),
    raw_payload     JSONB,                                          -- 回调原始报文(审计)
    paid_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, provider, provider_tx_id)                   -- 同一交易号全局唯一=幂等
);
CREATE INDEX idx_ledger_tenant_created ON payment_ledger(tenant_id, created_at);
COMMENT ON TABLE  payment_ledger              IS '支付流水(M4-S1); 反二清: 仅审计记账, 资金经微信直清特约商户, 不沉淀平台; 禁软删';
COMMENT ON COLUMN payment_ledger.provider_tx_id IS '微信/支付宝交易号; UNIQUE(tenant_id,provider,provider_tx_id) 回调幂等核心';
COMMENT ON COLUMN payment_ledger.raw_payload    IS '回调原始报文(审计, JSONB)';


-- ------------------------------------------------------------
-- 触发器: UPDATE 时自动刷新 updated_at (共用函数 trg_set_updated_at,
-- 沿用 000001-000011 约定). 仅挂 sub_merchants / payments (有 updated_at);
-- payment_ledger 无 updated_at 不挂.
-- ------------------------------------------------------------
CREATE TRIGGER trg_sub_merchants_updated_at
    BEFORE UPDATE ON sub_merchants
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

CREATE TRIGGER trg_payments_updated_at
    BEFORE UPDATE ON payments
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();
