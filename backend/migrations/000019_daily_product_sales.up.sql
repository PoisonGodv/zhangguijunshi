-- ============================================================
-- 000019_daily_product_sales.up.sql
-- 数据质量域: 融合后每日商品销量 (BE-A-08, CONTRACT §9.3)
-- 依据: 技术方案 第5章 数据质量 (BE-A-08 三源融合每日销量)
-- 目标数据库: PostgreSQL 16
--
-- 范围:
--   daily_product_sales: 每租户每店每商品每自然日的融合销量。
--     dataquality cron 反推 sale_out 后 upsert 落库(三源融合之一),
--     为 BE-A-09 滞销判定(DaysSinceLastSale)与 AI 管线提供数据。
--
-- 约定(沿用 000001-000018):
--   * 时间戳 TIMESTAMPTZ(UTC); created_at / updated_at 默认 now()。
--   * tenant_id NOT NULL REFERENCES tenants(id), 行级隔离键。
--   * UNIQUE(tenant_id, store_id, product_id, sale_date) 用 CREATE UNIQUE INDEX
--     (非内联 UNIQUE(...), 已知 gotcha: 内联 UNIQUE 带 WHERE 非法)。
--   * store_id NOT NULL DEFAULT 0: 0 = 全店融合口径(cron 反推按租户汇总),
--     故不加 stores 外键; product_id 同理不加 products 外键(融合表允许
--     商品已归档/删除后仍留历史销量)。
--   * updated_at 挂 BEFORE UPDATE 触发器(共用 000001 的 trg_set_updated_at)。
--   * 数量 BIGINT 非负, 禁浮点。
-- ============================================================

CREATE TABLE daily_product_sales (
    id          BIGSERIAL    PRIMARY KEY,
    tenant_id   BIGINT       NOT NULL REFERENCES tenants(id),
    store_id    BIGINT       NOT NULL DEFAULT 0,          -- 0 = 全店融合口径
    product_id  BIGINT       NOT NULL,
    sale_date   DATE         NOT NULL,
    qty         BIGINT       NOT NULL,                    -- 当日融合销量
    source      TEXT         NOT NULL DEFAULT 'sale_out', -- sale_out|manual|pos 融合来源
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK (qty >= 0)
);

CREATE UNIQUE INDEX idx_daily_product_sales_uq
    ON daily_product_sales(tenant_id, store_id, product_id, sale_date);
CREATE INDEX idx_daily_product_sales_tenant_date
    ON daily_product_sales(tenant_id, sale_date);

CREATE TRIGGER trg_daily_product_sales_updated_at
    BEFORE UPDATE ON daily_product_sales
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  daily_product_sales            IS '融合后每日商品销量(BE-A-08); UNIQUE(tenant_id,store_id,product_id,sale_date); dataquality cron upsert 覆盖';
COMMENT ON COLUMN daily_product_sales.id         IS '自增主键';
COMMENT ON COLUMN daily_product_sales.tenant_id  IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN daily_product_sales.store_id   IS '所属门店ID; 0 = 全店融合口径(cron 反推默认按租户汇总)';
COMMENT ON COLUMN daily_product_sales.product_id IS '商品ID(不加外键, 允许商品归档后留历史销量)';
COMMENT ON COLUMN daily_product_sales.sale_date  IS '销售自然日(UTC date)';
COMMENT ON COLUMN daily_product_sales.qty        IS '当日融合销量(按计价单位), 非负';
COMMENT ON COLUMN daily_product_sales.source     IS '融合来源: sale_out(库存反推) | manual | pos 等, 默认 sale_out';
COMMENT ON COLUMN daily_product_sales.created_at IS '创建时间(UTC)';
COMMENT ON COLUMN daily_product_sales.updated_at IS '更新时间(UTC), 由触发器自动刷新';
