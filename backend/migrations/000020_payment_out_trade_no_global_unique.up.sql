-- ============================================================
-- 000020_payment_out_trade_no_global_unique.up.sql
-- payments.out_trade_no 升级为全局唯一 + payment_ledger.status 放宽 CHECK。
-- 依据: payment 域 5 条 bug 修复 (bug #4 / #2)。
-- 目标数据库: PostgreSQL 16
--
-- 背景:
--   * out_trade_no 原为 UNIQUE(tenant_id, out_trade_no) 仅租户内唯一, 却被未鉴权
--     的微信支付回调 (CONTRACT §15.7) 当作"全局租户解析键": 回调不带租户, 服务端
--     靠 out_trade_no 反查归属租户 (GetByOutTradeNoAnyTenant)。一旦两个租户撞号,
--     租户解析即产生歧义 (可能把 A 的回调记到 B 的单上)。故升级为全局 UNIQUE,
--     从 schema 层根除歧义; 应用层 GetByOutTradeNoAnyTenant 仍对 >1 命中 fail-closed
--     (ErrAmbiguousOutTradeNo) 作纵深防御。
--   * payment_ledger.status 的 CHECK 原仅允许 success|pending|failed|refunded。
--     HandleNotify 新增"实付金额不符"审计: success 回调金额与支付单不一致时, 写一条
--     status='amount_mismatch' 的审计流水 (不置成功/不履约)。需放宽 CHECK 接纳该值。
--
-- 约定 (沿用 000001-000019):
--   * 唯一约束用 CREATE UNIQUE INDEX (不用内联 UNIQUE(...)WHERE, golang-migrate 内联
--     写法非法), 与 000018 idx_users_tenant_username 一致。
--   * 存量数据若存在跨租户撞号, 本 migration 会因唯一索引构建失败而中止 —— 这是预期
--     的 fail-fast (须先人工清理撞号行再重跑)。
-- ============================================================

-- 1. payments.out_trade_no 全局唯一。
--    保留既有 UNIQUE(tenant_id, out_trade_no) 表约束不动 (全局唯一是它的超集, 二者
--    并存无害; 删旧约束非必需且降低回滚风险)。新增全局唯一索引。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_payments_out_trade_no
    ON payments (out_trade_no);

COMMENT ON COLUMN payments.out_trade_no IS '商户订单号(业务侧生成), 全局唯一(uniq_payments_out_trade_no); 未鉴权回调据此反查归属租户, 故须全局唯一而非仅租户内唯一';

-- 2. payment_ledger.status 放宽 CHECK 以接纳 'amount_mismatch' 审计值。
--    先删旧约束再重建 (CHECK 约束名沿用建表时的默认命名 payment_ledger_status_check)。
ALTER TABLE payment_ledger
    DROP CONSTRAINT IF EXISTS payment_ledger_status_check;

ALTER TABLE payment_ledger
    ADD CONSTRAINT payment_ledger_status_check
    CHECK (status IN ('success','pending','failed','refunded','amount_mismatch'));

COMMENT ON COLUMN payment_ledger.status IS 'success|pending|failed|refunded|amount_mismatch; amount_mismatch=success回调实付与支付单金额不符的审计行(不置成功/不履约)';
