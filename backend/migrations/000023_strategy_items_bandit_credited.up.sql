-- ============================================================
-- 000023_strategy_items_bandit_credited.up.sql
-- bandit 计酬去重标记 (aiengine 三合一 修复③)
-- 依据: CONTRACT §14.7 (BanditUpdate 02:00 cron)
--
-- 背景(缺陷):
--   BanditUpdate 每 02:00 tick 枚举近 72h(attributionLookback)已执行
--   item 调 bandit.Learner.CreditItem 计酬。72h 窗口是为"漏 tick 补打"
--   保留的韧性, 但窗口内没有任何去重 —— 同一 item 会在连续多个 tick 被
--   重复计酬, 导致 arm.Trials 虚高、Q 估计被同一奖励反复拉动而失真。
--
-- 修复:
--   新增 bandit_credited_at TIMESTAMPTZ (NULL=尚未计酬)。去重责任放在
--   cron 调用方(pregen.creditTenant), 而非收紧枚举窗口:
--     * 枚举侧只取 bandit_credited_at IS NULL 的已执行 item(保留 72h
--       窗口对漏 tick 的补打韧性 —— 不收紧成 [上次跑点, now));
--     * CreditItem 成功后回写 bandit_credited_at=now(), 下一 tick 该
--       item 即被枚举过滤跳过。
--   bandit.Learner.Update 的累加语义不变(去重不侵入 bandit 包)。
--
-- 幂等: 列/索引 IF NOT EXISTS, 可安全重跑。
-- ============================================================

ALTER TABLE strategy_items
    ADD COLUMN IF NOT EXISTS bandit_credited_at TIMESTAMPTZ;

COMMENT ON COLUMN strategy_items.bandit_credited_at IS 'bandit 计酬去重标记: 首次 CreditItem 成功后回写 now(); NULL=尚未计酬; 枚举侧跳过非空(修复③, 保留72h补打韧性)';

-- 加速"未计酬的已执行 item"枚举过滤 (tenant + 非 pending + 未计酬)。
CREATE INDEX IF NOT EXISTS idx_strategy_items_bandit_uncredited
    ON strategy_items (tenant_id, execution_status)
    WHERE bandit_credited_at IS NULL AND execution_status != 'pending';
