-- ============================================================
-- 000003_billing_order.up.sql
-- 订单 / 记账域 (M1-S1)
-- 依据: 技术方案 第5章 §5.3.3 订单/记账对账域
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §8.1 裁剪):
--   纳入: orders + order_items
--   裁剪: payment_ledger / reconciliation_diffs 不建(支付对账属 M4),
--         留待后续迁移。
--
-- 分区降级(M1-S1 决策, 见 CONTRACT §8.1/§8.2):
--   ch05 中 orders / order_items 设计为 PARTITION BY RANGE (created_at),
--   主键写为 PRIMARY KEY (id, created_at)。本里程碑为可迁移性建【普通表】
--   (不带 PARTITION BY), 改用普通 BIGSERIAL PRIMARY KEY (id);
--   月分区滚动留待后续迁移(线上数据量未到分区阈值前普通表即够)。
--
-- 其它约定(沿用 000001):
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at / updated_at 默认 now()。
--   * orders 挂 BEFORE UPDATE 触发器刷新 updated_at
--     (共用函数 trg_set_updated_at 已在 000001 创建, 此处仅 CREATE TRIGGER)。
--   * order_items 无 updated_at 且不挂触发器。
--   * orders / order_items 强审计, 禁软删(无 deleted_at 列)。
--   * 金额一律 BIGINT 分, 禁浮点。
-- ============================================================


-- ============================================================
-- 1. orders : 订单(大流水表)
--    M1-S1 降级为普通表(非分区); 禁软删。
-- ============================================================
CREATE TABLE orders (
    id             BIGSERIAL    PRIMARY KEY,
    uuid           UUID         NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id      BIGINT       NOT NULL REFERENCES tenants(id),
    store_id       BIGINT       NOT NULL REFERENCES stores(id),
    member_id      BIGINT,                                 -- 会员(可空, 现金散客)
    order_no       TEXT         NOT NULL,                  -- 对外单号
    channel        TEXT         NOT NULL
                   CHECK (channel IN ('in_store','wxshop','wework','delivery')),
    status         TEXT         NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','paid','refunded','voided')),
    total_cents    BIGINT       NOT NULL DEFAULT 0,        -- 应收(分)
    paid_cents     BIGINT       NOT NULL DEFAULT 0,        -- 实收(分)
    discount_cents BIGINT       NOT NULL DEFAULT 0,        -- 优惠抵扣(分)
    payment_method TEXT,                                   -- wxpay|alipay|cash|balance|mixed
    operator_id    BIGINT       REFERENCES users(id),
    paid_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, order_no)
);

CREATE INDEX idx_orders_tenant_store_created ON orders(tenant_id, store_id, created_at);
CREATE INDEX idx_orders_tenant_member ON orders(tenant_id, member_id) WHERE member_id IS NOT NULL;

CREATE TRIGGER trg_orders_updated_at
    BEFORE UPDATE ON orders
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  orders                IS '订单表; 强审计禁软删; M1-S1 普通表, 月分区滚动留待后续迁移';
COMMENT ON COLUMN orders.id             IS '订单自增主键';
COMMENT ON COLUMN orders.uuid           IS '对外公开ID, 不暴露自增序号';
COMMENT ON COLUMN orders.tenant_id      IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN orders.store_id       IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN orders.member_id      IS '会员ID, 可空(现金散客)';
COMMENT ON COLUMN orders.order_no       IS '对外单号, 租户内唯一';
COMMENT ON COLUMN orders.channel        IS '下单渠道: in_store(门店) | wxshop(小程序) | wework(企微) | delivery(外卖)';
COMMENT ON COLUMN orders.status         IS '订单状态: pending(待支付) | paid(已支付) | refunded(已退款) | voided(已作废)';
COMMENT ON COLUMN orders.total_cents    IS '应收金额(分)';
COMMENT ON COLUMN orders.paid_cents     IS '实收金额(分)';
COMMENT ON COLUMN orders.discount_cents IS '优惠抵扣金额(分)';
COMMENT ON COLUMN orders.payment_method IS '支付方式: wxpay | alipay | cash | balance | mixed';
COMMENT ON COLUMN orders.operator_id    IS '操作人ID(外键 -> users.id), 可空';
COMMENT ON COLUMN orders.paid_at        IS '支付完成时间(UTC), 可空';
COMMENT ON COLUMN orders.created_at     IS '创建时间(UTC)';
COMMENT ON COLUMN orders.updated_at     IS '更新时间(UTC), 由触发器自动刷新';


-- ============================================================
-- 2. order_items : 订单明细
--    M1-S1 降级为普通表(非分区); 无 updated_at 不挂触发器; 禁软删。
-- ============================================================
CREATE TABLE order_items (
    id            BIGSERIAL    PRIMARY KEY,
    tenant_id     BIGINT       NOT NULL REFERENCES tenants(id),
    order_id      BIGINT       NOT NULL REFERENCES orders(id),
    product_id    BIGINT       NOT NULL REFERENCES products(id),
    batch_id      BIGINT       REFERENCES inventory_batches(id),
    name_snapshot TEXT         NOT NULL,                   -- 商品名快照(防商品改名)
    unit          TEXT         NOT NULL,
    qty           BIGINT       NOT NULL,
    price_cents   BIGINT       NOT NULL,                   -- 成交单价(分)
    cost_cents    BIGINT       NOT NULL DEFAULT 0,         -- 成本(分, 归因利润用)
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_order_items_tenant_order ON order_items(tenant_id, order_id);
CREATE INDEX idx_order_items_tenant_product ON order_items(tenant_id, product_id);

COMMENT ON TABLE  order_items               IS '订单明细表; 强审计禁软删; M1-S1 普通表, 月分区滚动留待后续迁移';
COMMENT ON COLUMN order_items.id            IS '明细自增主键';
COMMENT ON COLUMN order_items.tenant_id     IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN order_items.order_id      IS '所属订单ID(外键 -> orders.id)';
COMMENT ON COLUMN order_items.product_id    IS '商品ID(外键 -> products.id)';
COMMENT ON COLUMN order_items.batch_id      IS '关联批次ID(外键 -> inventory_batches.id), 可空';
COMMENT ON COLUMN order_items.name_snapshot IS '商品名快照(防商品改名)';
COMMENT ON COLUMN order_items.unit          IS '计价单位快照';
COMMENT ON COLUMN order_items.qty           IS '成交数量';
COMMENT ON COLUMN order_items.price_cents   IS '成交单价(分)';
COMMENT ON COLUMN order_items.cost_cents    IS '成本(分), 归因利润用';
COMMENT ON COLUMN order_items.created_at    IS '创建时间(UTC)';
