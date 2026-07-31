-- ============================================================
-- 000021_subscribe_auth_unique.down.sql
-- 逆序回滚 000021: 去掉 subscribe_auth 的 (tenant_id, member_id, template_id)
-- 唯一索引, 回到仅有非唯一索引 idx_subauth_tenant_member 的状态。
-- 注意: up 中的重复行合并 (SUM 折叠) 不可逆 —— 回滚只去唯一性, 不恢复被删的重复行。
-- 既有非唯一索引 idx_subauth_tenant_member 在 up 中未动, 无需恢复。
-- ============================================================

DROP INDEX IF EXISTS uq_subauth_tenant_member_tpl;
