-- 000013_subscription.down.sql
-- 逆序: DROP TABLE subscriptions (触发器随表级联删除, 无需单独 DROP TRIGGER)。
DROP TABLE IF EXISTS subscriptions;
