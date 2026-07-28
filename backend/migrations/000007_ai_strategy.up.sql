-- ============================================================
-- 000007_ai_strategy.up.sql
-- AI 引擎 / 归因域 (M3-S1, DB-04)
-- 依据: 技术方案 第5章 §5.3.8 (AI 引擎/归因域 DDL, 行 532-595)
--       CONTRACT §12.2 (M3-S1 迁移编号与有意增补)
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §12.1, 4 张表全部纳入):
--   今日策略主表:    strategies
--   策略单项(3件事): strategy_items
--   归因回填(M3-S3):strategy_attribution (本期建表空)
--   商家显式反馈:    strategy_feedback
--
-- 分区: ch05 无分区设计, 均普通表。
--
-- 触发器: 4 张表均【无 updated_at】—— 策略每日 upsert 覆盖
--   (uniq_strategy_daily), 单向审计流, 不挂 trg_set_updated_at。
--
-- 有意增补(对照 CONTRACT §12.2, 服务于 action_type 双层设计):
--   1. strategy_items 增 reason_type TEXT NOT NULL 列 + CHECK 7 值
--      (clearance_coupon|rain_delivery|bundle_slow|wake_dormant|
--       festival_discount|birthday_care|high_value_push)。
--      ch05 §5.3.8 原始 DDL 无此列; reason_type 是规则库「为什么」的落库
--      字段, 与 action_type(执行通道)解耦(CONTRACT §12.3.0/§12.5.9)。
--   2. strategy_items.execution_status CHECK 在 ch05 四值基础上加 'blocked'
--      (pending|executed|skipped|failed|blocked), 承载一键执行被合规
--      拦截的归因态(CONTRACT §12.5.7)。
--
-- 其它约定(沿用 000001-000006):
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at 默认 now()。
--   * tenant_id 全部 NOT NULL REFERENCES tenants(id), 行级隔离键。
--   * 金额 token 成本走 total_cost_microcents BIGINT (1元=10000微分,
--     便于聚合; 非营销金额分, 仅成本聚合用)。
--   * signals / target / params / expected_effect 用 JSONB 存弹性结构。
-- ============================================================


-- ============================================================
-- 1. strategies : 今日策略主表(每日 upsert 覆盖)
--    generated_by: cache_hit | offline_pregen(M3-S2) | live_llm | rule_only
--    status:       generated | partially_executed | executed | dismissed
--    UNIQUE(tenant_id, store_id, for_date): 同租户同店同日仅一条,
--                  重新生成覆盖(CONTRACT §12.5.8 SaveAdvice upsert)。
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE strategies (
    id                  BIGSERIAL    PRIMARY KEY,
    uuid                UUID         NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    tenant_id           BIGINT       NOT NULL REFERENCES tenants(id),
    store_id            BIGINT       NOT NULL REFERENCES stores(id),
    for_date            DATE         NOT NULL,
    signals             JSONB        NOT NULL,                       -- 当日 signal.Bundle 快照
    data_quality_score  INT          NOT NULL,                       -- 0-100, 应用层约束范围
    generated_by        TEXT         NOT NULL
                        CHECK (generated_by IN ('cache_hit','offline_pregen','live_llm','rule_only')),
    llm_tier            TEXT,                                        -- doubao_lite|deepseek_v3|deepseek_r1|qwen; rule_only 时 NULL
    total_cost_microcents BIGINT     NOT NULL DEFAULT 0,             -- 本次 token 成本(1元=10000微分)
    status              TEXT         NOT NULL DEFAULT 'generated'
                        CHECK (status IN ('generated','partially_executed','executed','dismissed')),
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK (data_quality_score BETWEEN 0 AND 100)
);

CREATE UNIQUE INDEX uniq_strategy_daily ON strategies(tenant_id, store_id, for_date);
CREATE INDEX idx_strategies_tenant_date ON strategies(tenant_id, for_date);
CREATE INDEX idx_strategies_tenant_status ON strategies(tenant_id, status);

COMMENT ON TABLE  strategies                       IS '今日策略主表(每日 upsert 覆盖); uniq_strategy_daily 兜底同租户同店同日仅一条; 无 updated_at';
COMMENT ON COLUMN strategies.id                    IS '策略自增主键';
COMMENT ON COLUMN strategies.uuid                  IS '对外 UUID, gen_random_uuid() 生成';
COMMENT ON COLUMN strategies.tenant_id             IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN strategies.store_id              IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN strategies.for_date              IS '策略所属自然日(UTC date)';
COMMENT ON COLUMN strategies.signals               IS '当日 signal.Bundle 快照 JSONB(strategy 包不 import signal, opaque 落库)';
COMMENT ON COLUMN strategies.data_quality_score    IS '数据质量分 0-100, 仅 trace/降级判定';
COMMENT ON COLUMN strategies.generated_by          IS '生成来源: cache_hit | offline_pregen(M3-S2) | live_llm | rule_only';
COMMENT ON COLUMN strategies.llm_tier              IS 'LLM 档位; rule_only/cache_hit 时 NULL';
COMMENT ON COLUMN strategies.total_cost_microcents IS '本次 token 成本(1元=10000微分, 便于聚合), 非营销金额分';
COMMENT ON COLUMN strategies.status                IS '策略态: generated | partially_executed | executed | dismissed';
COMMENT ON COLUMN strategies.created_at            IS '创建时间(UTC); 同日 upsert 保留首建时间';


-- ============================================================
-- 2. strategy_items : 今日3件事单项(强归因, 禁软删)
--    rank: 1-3(CONTRACT §12.3.1)
--    action_type: 6 值执行通道(CONTRACT §12.3.0)
--    reason_type: 7 值规则语义(本期增补, CONTRACT §12.3.0)
--    execution_status: pending | executed | skipped | failed | blocked(本期增补 blocked)
--    UNIQUE(tenant_id, strategy_id, rank): 同策略内 rank 唯一。
--    无 created_at / updated_at, 不挂触发器; 仅 executed_at。
-- ============================================================
CREATE TABLE strategy_items (
    id               BIGSERIAL    PRIMARY KEY,
    tenant_id        BIGINT       NOT NULL REFERENCES tenants(id),
    strategy_id      BIGINT       NOT NULL REFERENCES strategies(id),
    rank             INT          NOT NULL CHECK (rank BETWEEN 1 AND 3),
    action_type      TEXT         NOT NULL
                     CHECK (action_type IN ('group_seckill','coupon_wake','moments_post','price_adj','groupbuy','recharge_promo')),
    reason_type      TEXT         NOT NULL
                     CHECK (reason_type IN ('clearance_coupon','rain_delivery','bundle_slow','wake_dormant','festival_discount','birthday_care','high_value_push')),
    target           JSONB,                                       -- 受众筛选条件
    product_id       BIGINT       REFERENCES products(id),         -- 可空(非商品绑定)
    params           JSONB        NOT NULL DEFAULT '{}'::jsonb,    -- 折扣/满减/文案等
    copy             TEXT,                                        -- ≤40字口语化文案
    reason           TEXT,                                        -- 推荐理由(老板可读)
    expected_effect  JSONB,                                       -- 预期核销X/到店Y/增收Z
    margin_ok        BOOLEAN      NOT NULL DEFAULT TRUE,           -- 毛利护栏校验
    execution_status TEXT         NOT NULL DEFAULT 'pending'
                     CHECK (execution_status IN ('pending','executed','skipped','failed','blocked')),
    executed_at      TIMESTAMPTZ,
    UNIQUE (tenant_id, strategy_id, rank)
);

CREATE INDEX idx_strategy_items_strategy ON strategy_items(tenant_id, strategy_id);
CREATE INDEX idx_strategy_items_status ON strategy_items(tenant_id, execution_status) WHERE execution_status != 'pending';

COMMENT ON TABLE  strategy_items              IS '策略单项(今日3件事); UNIQUE(tenant_id,strategy_id,rank); execution_status 含本期增补 blocked; 无 created_at/updated_at';
COMMENT ON COLUMN strategy_items.id           IS '单项自增主键';
COMMENT ON COLUMN strategy_items.tenant_id    IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN strategy_items.strategy_id  IS '所属策略ID(外键 -> strategies.id)';
COMMENT ON COLUMN strategy_items.rank         IS '优先级序号 1-3';
COMMENT ON COLUMN strategy_items.action_type  IS '执行通道 6 值: group_seckill | coupon_wake | moments_post | price_adj | groupbuy | recharge_promo';
COMMENT ON COLUMN strategy_items.reason_type  IS '规则语义 7 值(本期增补): clearance_coupon | rain_delivery | bundle_slow | wake_dormant | festival_discount | birthday_care | high_value_push';
COMMENT ON COLUMN strategy_items.target       IS '受众筛选 JSONB, 可空';
COMMENT ON COLUMN strategy_items.product_id   IS '关联商品ID(外键 -> products.id), 可空(非商品绑定)';
COMMENT ON COLUMN strategy_items.params       IS '弹性参数 JSONB: 折扣/满减/文案/限份等';
COMMENT ON COLUMN strategy_items.copy         IS '老板可读口语化文案, ≤40字';
COMMENT ON COLUMN strategy_items.reason       IS '推荐理由(老板可读)';
COMMENT ON COLUMN strategy_items.expected_effect IS '预期效果 JSONB: 核销X/到店Y/增收Z';
COMMENT ON COLUMN strategy_items.margin_ok    IS '毛利护栏校验结果, 默认 TRUE';
COMMENT ON COLUMN strategy_items.execution_status IS '执行态: pending | executed | skipped | failed | blocked(blocked 为本期增补, 合规拦截归因)';
COMMENT ON COLUMN strategy_items.executed_at  IS '执行/拦截/跳过/失败时间(UTC); pending 时 NULL';


-- ============================================================
-- 3. strategy_attribution : 策略执行归因回填(M3-S3 回填, 本期建表空)
--    metric: 5 值效果指标
--    UNIQUE(tenant_id, strategy_item_id, metric, measured_at): 同指标多次度量留痕。
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE strategy_attribution (
    id               BIGSERIAL    PRIMARY KEY,
    tenant_id        BIGINT       NOT NULL REFERENCES tenants(id),
    strategy_item_id BIGINT       NOT NULL REFERENCES strategy_items(id),
    metric           TEXT         NOT NULL
                     CHECK (metric IN ('coupon_redeem','visit_uplift','groupbuy_success','repurchase','moments_engage')),
    value            DOUBLE PRECISION,
    measured_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, strategy_item_id, metric, measured_at)
);

CREATE INDEX idx_attribution_item ON strategy_attribution(tenant_id, strategy_item_id);

COMMENT ON TABLE  strategy_attribution                  IS '策略执行归因回填(M3-S3 回填, 本期建表空); UNIQUE(tenant_id,strategy_item_id,metric,measured_at)';
COMMENT ON COLUMN strategy_attribution.id               IS '归因记录自增主键';
COMMENT ON COLUMN strategy_attribution.tenant_id        IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN strategy_attribution.strategy_item_id IS '策略单项ID(外键 -> strategy_items.id)';
COMMENT ON COLUMN strategy_attribution.metric           IS '效果指标: coupon_redeem | visit_uplift | groupbuy_success | repurchase | moments_engage';
COMMENT ON COLUMN strategy_attribution.value            IS '度量值, 可空';
COMMENT ON COLUMN strategy_attribution.measured_at      IS '度量时间(UTC)';


-- ============================================================
-- 4. strategy_feedback : 商家显式反馈(有用/没用, 反哺规则权重)
--    UNIQUE(tenant_id, strategy_item_id): 一策略单项仅一条反馈,
--                  重复反馈覆盖(UPDATE useful)。CONTRACT §12.3.1 RecordFeedback。
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE strategy_feedback (
    id               BIGSERIAL    PRIMARY KEY,
    tenant_id        BIGINT       NOT NULL REFERENCES tenants(id),
    strategy_item_id BIGINT       NOT NULL REFERENCES strategy_items(id),
    useful           BOOLEAN      NOT NULL,                        -- true=👍 false=👎
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, strategy_item_id)
);

CREATE INDEX idx_feedback_item ON strategy_feedback(tenant_id, strategy_item_id);

COMMENT ON TABLE  strategy_feedback                  IS '商家显式反馈; UNIQUE(tenant_id,strategy_item_id) 兜底重复反馈覆盖语义';
COMMENT ON COLUMN strategy_feedback.id               IS '反馈自增主键';
COMMENT ON COLUMN strategy_feedback.tenant_id        IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN strategy_feedback.strategy_item_id IS '策略单项ID(外键 -> strategy_items.id)';
COMMENT ON COLUMN strategy_feedback.useful           IS '有用标记: true=👍 false=👎';
COMMENT ON COLUMN strategy_feedback.created_at       IS '反馈时间(UTC); 重复反馈时由应用层刷新';
