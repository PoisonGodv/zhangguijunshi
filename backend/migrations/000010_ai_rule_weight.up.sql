-- ============================================================
-- 000010_ai_rule_weight.up.sql
-- AI 引擎 / 规则权重 bandit 自学习持久化 (M3-S3, AI-12)
-- 依据: 技术方案 第7章 §7.9.2 / CONTRACT §14.2 (M3-S3 迁移编号 DB-06)
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §14.1):
--   bandit epsilon-greedy 自学习的 Arm 持久化表. Learner.Update 软更新
--   Weight(Q 值), pregen bandit-update cron + strategy_feedback 反哺统一吃.
--
-- arm key = reason_type (7 值, cold-start-safe, 每业态必命中) 或 rule.ID
-- (本期 reason_type 7 arm); tenant_id=0 留作平台默认(本期不用).
--
-- 其它约定(沿用 000001-000009):
--   * 时间戳统一 TIMESTAMPTZ(UTC); updated_at 默认 now().
--   * tenant_id NOT NULL REFERENCES tenants(id), 行级隔离键.
--   * updated_at 由 BEFORE UPDATE 触发器自动刷新(共用函数 trg_set_updated_at).
-- ============================================================


-- ------------------------------------------------------------
-- 1. ai_rule_weight : 规则权重 bandit 自学习表
--    UNIQUE(tenant_id, rule_id): 同租户同 rule 仅一条, upsert 覆盖.
--    Weight 初值 0.5 (中性先验, 未见规则 cold-start-safe).
-- ------------------------------------------------------------
CREATE TABLE ai_rule_weight (
    id          BIGSERIAL      PRIMARY KEY,
    tenant_id   BIGINT         NOT NULL REFERENCES tenants(id),
    rule_id     TEXT           NOT NULL,                       -- reason_type (7 值) 或 rule.ID
    weight      DOUBLE PRECISION NOT NULL DEFAULT 0.5,        -- 软更新后的 Q 值, 初值 0.5 中性先验
    trials      INT            NOT NULL DEFAULT 0,
    reward_sum  DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ    NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, rule_id)
);

CREATE INDEX idx_ai_rule_weight_tenant ON ai_rule_weight(tenant_id);

COMMENT ON TABLE  ai_rule_weight            IS '规则权重 bandit 自学习(M3-S3); arm=reason_type; UNIQUE(tenant_id,rule_id)';
COMMENT ON COLUMN ai_rule_weight.tenant_id  IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN ai_rule_weight.rule_id    IS 'arm key = reason_type(7 值) 或 rule.ID; 本期 reason_type';
COMMENT ON COLUMN ai_rule_weight.weight     IS '软更新 Q 值, 初值 0.5 中性先验(未见规则 cold-start-safe)';
COMMENT ON COLUMN ai_rule_weight.trials     IS '已施加的 reward 次数(增量均值分母)';
COMMENT ON COLUMN ai_rule_weight.reward_sum IS 'clipped reward 累计和, q=reward_sum/trials';
COMMENT ON COLUMN ai_rule_weight.updated_at IS '更新时间(UTC), 由触发器自动刷新';

-- ------------------------------------------------------------
-- 触发器: UPDATE 时自动刷新 updated_at (共用函数 trg_set_updated_at,
-- 沿用 000001-000009 约定; bandit 每次 Update 刷新 updated_at).
-- ------------------------------------------------------------
CREATE TRIGGER trg_ai_rule_weight_updated_at
    BEFORE UPDATE ON ai_rule_weight
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();
