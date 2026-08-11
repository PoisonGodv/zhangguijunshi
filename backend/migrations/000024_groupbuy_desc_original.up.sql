-- ============================================================
-- 000024_groupbuy_desc_original.up.sql
-- groupbuy 独立 desc / 原价字段 (B2 增量, BE-C, 缺口 5)
-- 依据: CONTRACT §24.6
--
-- 背景(缺陷):
--   groupbuys 表此前没有独立描述/原价列, C 端 groupbuyDTO 只能降级:
--   desc 回退 title、original_cents 回退 params->>'price_cents'(团购价),
--   商家无法展示"原价 vs 团购价"的划线价对比, 也无法写独立营销文案。
--
-- 修复:
--   description TEXT NOT NULL DEFAULT ''   —— 独立描述文案;
--     '' = 未设置, C 端 (wxshop groupbuyDetailDTO) 保持降级回退 title。
--   original_price_cents BIGINT NOT NULL DEFAULT 0 —— 原价(分);
--     0 = 未设置, C 端保持降级回退团购价 (params.price_cents)。
--
--   NOT NULL + DEFAULT 保证存量行零迁移成本且天然携带降级语义
--   (旧行 desc='' / original=0 → C 端仍走 title / 团购价回退)。
--
-- 幂等: IF NOT EXISTS, 可安全重跑。
-- ============================================================

ALTER TABLE groupbuys
    ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';

ALTER TABLE groupbuys
    ADD COLUMN IF NOT EXISTS original_price_cents BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN groupbuys.description IS '拼团独立描述文案(空=回退 title)';
COMMENT ON COLUMN groupbuys.original_price_cents IS '原价(分),0=未设置(前端回退=团购价)';
