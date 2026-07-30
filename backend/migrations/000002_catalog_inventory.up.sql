-- ============================================================
-- 000002_catalog_inventory.up.sql
-- 商品 / 库存域 (M1-S1)
-- 依据: 技术方案 第5章 §5.3.2 商品/库存域
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §8.1 裁剪):
--   纳入: products + inventory_batches + inventory_movements
--   裁剪: product_pending(多源采集) 不建, 留待 M1-S3 迁移。
--
-- 分区降级(M1-S1 决策, 见 CONTRACT §8.1/§8.2):
--   ch05 中 inventory_movements 设计为 PARTITION BY RANGE (created_at),
--   主键写为 PRIMARY KEY (id, created_at)。本里程碑为可迁移性建【普通表】
--   (不带 PARTITION BY), 改用普通 BIGSERIAL PRIMARY KEY (id);
--   月分区滚动留待后续迁移(线上数据量未到分区阈值前普通表即够)。
--
-- 其它约定(沿用 000001):
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at / updated_at 默认 now()。
--   * products / inventory_batches 挂 BEFORE UPDATE 触发器刷新 updated_at
--     (共用函数 trg_set_updated_at 已在 000001 创建, 此处仅 CREATE TRIGGER)。
--   * inventory_movements 为强审计流水, 无 updated_at 且不挂触发器, 禁软删。
--   * 金额一律 BIGINT 分, 禁浮点。
-- ============================================================


-- ============================================================
-- 1. products : 商品主档
-- ============================================================
CREATE TABLE products (
    id              BIGSERIAL    PRIMARY KEY,
    uuid            UUID         NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id       BIGINT       NOT NULL REFERENCES tenants(id),
    store_id        BIGINT       NOT NULL REFERENCES stores(id),
    barcode         TEXT,                                  -- EAN-13/店内码
    name            TEXT         NOT NULL,
    category        TEXT,
    unit            TEXT         NOT NULL DEFAULT '个',    -- 斤/个/盒
    cost_cents      BIGINT       NOT NULL DEFAULT 0,       -- 成本(分)
    price_cents     BIGINT       NOT NULL DEFAULT 0,       -- 售价(分)
    image_url       TEXT,
    shelf_life_days INT,                                   -- 保质期天数(生鲜/母婴)
    tags            TEXT[]       DEFAULT ARRAY[]::TEXT[],  -- 临期|滞销|高毛利
    status          TEXT         NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active','archived')),
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_products_tenant_store_barcode
    ON products (tenant_id, store_id, barcode) WHERE deleted_at IS NULL AND barcode IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_products_tenant_store_name_unit
    ON products (tenant_id, store_id, name, unit) WHERE deleted_at IS NULL;

CREATE INDEX idx_products_tenant_store ON products(tenant_id, store_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_products_tags ON products(tenant_id, store_id) WHERE deleted_at IS NULL AND array_length(tags,1) > 0;

CREATE TRIGGER trg_products_updated_at
    BEFORE UPDATE ON products
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  products                 IS '商品主档; 按 tenant_id + store_id 行级隔离';
COMMENT ON COLUMN products.id              IS '商品自增主键';
COMMENT ON COLUMN products.uuid            IS '对外公开ID, 不暴露自增序号';
COMMENT ON COLUMN products.tenant_id       IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN products.store_id        IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN products.barcode         IS '条码: EAN-13 或店内码, 可空';
COMMENT ON COLUMN products.name            IS '商品名称';
COMMENT ON COLUMN products.category        IS '商品分类';
COMMENT ON COLUMN products.unit            IS '计价单位: 斤/个/盒 等, 默认 个';
COMMENT ON COLUMN products.cost_cents      IS '成本(分)';
COMMENT ON COLUMN products.price_cents     IS '售价(分)';
COMMENT ON COLUMN products.image_url       IS '商品图 COS URL';
COMMENT ON COLUMN products.shelf_life_days IS '保质期天数(生鲜/母婴), NULL 表示未设置';
COMMENT ON COLUMN products.tags            IS '标签数组: 临期|滞销|高毛利 等';
COMMENT ON COLUMN products.status          IS '商品状态: active(在售) | archived(归档)';
COMMENT ON COLUMN products.created_at      IS '创建时间(UTC)';
COMMENT ON COLUMN products.updated_at      IS '更新时间(UTC), 由触发器自动刷新';
COMMENT ON COLUMN products.deleted_at      IS '软删除时间, NULL 表示未删除';


-- ============================================================
-- 2. inventory_batches : 库存批次(按进货批次, 支持临期管理)
-- ============================================================
CREATE TABLE inventory_batches (
    id               BIGSERIAL    PRIMARY KEY,
    tenant_id        BIGINT       NOT NULL REFERENCES tenants(id),
    store_id         BIGINT       NOT NULL REFERENCES stores(id),
    product_id       BIGINT       NOT NULL REFERENCES products(id),
    quantity         BIGINT       NOT NULL DEFAULT 0,      -- 当前余量(按单位)
    in_price_cents   BIGINT       NOT NULL DEFAULT 0,      -- 该批次进货价(分)
    expiry_date      DATE,                                 -- 到期日
    expiry_warn_date DATE,                                 -- 临期预警日(expiry_date - N)
    status           TEXT         NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active','nearing','expired','cleared')),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK (quantity >= 0)
);

CREATE INDEX idx_inv_tenant_store ON inventory_batches(tenant_id, store_id, product_id);
CREATE INDEX idx_inv_nearing ON inventory_batches(tenant_id, store_id) WHERE status='nearing';

CREATE TRIGGER trg_inventory_batches_updated_at
    BEFORE UPDATE ON inventory_batches
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  inventory_batches                  IS '库存批次表; 按进货批次管理, 支持临期预警';
COMMENT ON COLUMN inventory_batches.id               IS '批次自增主键';
COMMENT ON COLUMN inventory_batches.tenant_id        IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN inventory_batches.store_id         IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN inventory_batches.product_id       IS '商品ID(外键 -> products.id)';
COMMENT ON COLUMN inventory_batches.quantity         IS '当前余量(按计价单位), 非负';
COMMENT ON COLUMN inventory_batches.in_price_cents   IS '该批次进货价(分)';
COMMENT ON COLUMN inventory_batches.expiry_date      IS '到期日';
COMMENT ON COLUMN inventory_batches.expiry_warn_date IS '临期预警日(expiry_date - N)';
COMMENT ON COLUMN inventory_batches.status           IS '批次状态: active(正常) | nearing(临期) | expired(过期) | cleared(清完)';
COMMENT ON COLUMN inventory_batches.created_at       IS '创建时间(UTC)';
COMMENT ON COLUMN inventory_batches.updated_at       IS '更新时间(UTC), 由触发器自动刷新';


-- ============================================================
-- 3. inventory_movements : 库存流水(出入库, 强审计, 禁软删)
--    M1-S1 降级为普通表(非分区); 无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE inventory_movements (
    id            BIGSERIAL    PRIMARY KEY,
    tenant_id     BIGINT       NOT NULL REFERENCES tenants(id),
    store_id      BIGINT       NOT NULL REFERENCES stores(id),
    product_id    BIGINT       NOT NULL REFERENCES products(id),
    batch_id      BIGINT       REFERENCES inventory_batches(id),
    movement_type TEXT         NOT NULL
                  CHECK (movement_type IN ('purchase_in','sale_out','stocktake_adj','return_in','loss','transfer')),
    quantity      BIGINT       NOT NULL,                   -- 正入负出
    ref_order_id  BIGINT,                                  -- 关联订单(销售/退货)
    operator_id   BIGINT       REFERENCES users(id),
    note          TEXT,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_invmov_tenant_store_product ON inventory_movements(tenant_id, store_id, product_id);
CREATE INDEX idx_invmov_tenant_created ON inventory_movements(tenant_id, created_at);

COMMENT ON TABLE  inventory_movements               IS '库存流水表(出入库), 强审计禁软删; M1-S1 普通表, 月分区滚动留待后续迁移';
COMMENT ON COLUMN inventory_movements.id            IS '流水自增主键';
COMMENT ON COLUMN inventory_movements.tenant_id     IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN inventory_movements.store_id      IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN inventory_movements.product_id    IS '商品ID(外键 -> products.id)';
COMMENT ON COLUMN inventory_movements.batch_id      IS '关联批次ID(外键 -> inventory_batches.id), 可空';
COMMENT ON COLUMN inventory_movements.movement_type IS '出入库类型: purchase_in(采购入库) | sale_out(销售出库) | stocktake_adj(盘点调整) | return_in(退货入库) | loss(报损) | transfer(调拨)';
COMMENT ON COLUMN inventory_movements.quantity      IS '数量, 正数入库负数出库';
COMMENT ON COLUMN inventory_movements.ref_order_id  IS '关联订单ID(销售/退货), 可空';
COMMENT ON COLUMN inventory_movements.operator_id   IS '操作人ID(外键 -> users.id), 可空';
COMMENT ON COLUMN inventory_movements.note          IS '备注';
COMMENT ON COLUMN inventory_movements.created_at    IS '创建时间(UTC)';
