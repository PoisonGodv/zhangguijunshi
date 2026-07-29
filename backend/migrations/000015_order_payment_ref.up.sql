-- 000015: 补 orders.payment_ref 列。
-- M4-S1 给领域类型 Order.PaymentRef(billing.go) 加了字段(pending→paid 推进时填关联支付单
-- out_trade_no)，但当时 billing 全走 memory，没人给 orders 表补对应列；pgx 化 billing 时
-- (CONTRACT §17) 该 gap 暴露：PGOrderRepo 把 payment_ref 当列持久化(求生产可用 + 与
-- MemoryOrderRepo drop-in 等价)，故此 migration 落地该列。
ALTER TABLE orders ADD COLUMN IF NOT EXISTS payment_ref TEXT;
COMMENT ON COLUMN orders.payment_ref IS '关联支付单 out_trade_no(M4-S1 pending→paid 推进时填; 现金单空; 幂等回调不覆盖)';
