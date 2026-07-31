-- ============================================================
-- 000020_payment_out_trade_no_global_unique.down.sql
-- 逆序回滚 000020: 去掉 payments.out_trade_no 全局唯一索引, 恢复
-- payment_ledger.status 原 CHECK (不含 amount_mismatch)。
-- 既有 UNIQUE(tenant_id, out_trade_no) 表约束在 up 中未动, 无需恢复。
-- ============================================================

-- 1. 去掉全局唯一索引, 回到仅租户内唯一。
DROP INDEX IF EXISTS uniq_payments_out_trade_no;

-- 2. 恢复 payment_ledger.status 原 CHECK。
ALTER TABLE payment_ledger
    DROP CONSTRAINT IF EXISTS payment_ledger_status_check;

ALTER TABLE payment_ledger
    ADD CONSTRAINT payment_ledger_status_check
    CHECK (status IN ('success','pending','failed','refunded'));
