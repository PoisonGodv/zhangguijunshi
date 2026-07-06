-- ============================================================
-- 000001_init_tenant_user_store.down.sql
-- 回滚 000001: 逆序删除 users -> stores -> tenants, 最后删共用触发器函数
-- 依赖关系: users.store_id -> stores.id, users.tenant_id -> tenants.id,
--           stores.tenant_id -> tenants.id; 故删表顺序必须为 users, stores, tenants。
-- 删表会级联删除挂在其上的 BEFORE UPDATE 触发器; 之后函数不再被引用, 安全 DROP。
-- ============================================================

DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS stores;
DROP TABLE IF EXISTS tenants;

DROP FUNCTION IF EXISTS trg_set_updated_at();
