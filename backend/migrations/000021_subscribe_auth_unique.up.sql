-- ============================================================
-- 000021_subscribe_auth_unique.up.sql
-- subscribe_auth 增加 (tenant_id, member_id, template_id) 唯一性。
-- 依据: reach 域 bug 修复 (#3) —— GrantSubscribe 原为"读后写"且表上无 UNIQUE,
--       并发首授会插重复行, 破坏一次性授权配额记账 (同一 (租户,会员,模板) 出现多行,
--       ConsumeSubscribe 只更新其中一行, 配额被重复/漏记)。
-- 目标数据库: PostgreSQL 16
--
-- 背景:
--   * subscribe_auth (000006) 仅有非唯一索引 idx_subauth_tenant_member
--     (tenant_id, member_id, template_id), 无 UNIQUE。应用层 GrantSubscribe 改为
--     单语句 `INSERT ... ON CONFLICT (tenant_id, member_id, template_id) DO UPDATE`
--     upsert, 需要本唯一索引作为冲突目标 (ON CONFLICT 可命中唯一索引, 无需命名约束)。
--   * 存量若已有重复行, 直接建唯一索引会失败。故先按 (tenant_id, member_id,
--     template_id) 合并: 保留 MIN(id) 一行, granted_quota / used_quota 取 SUM,
--     granted_at 取 MAX, 再删其余重复行, 最后建唯一索引。
--
-- 约定 (沿用 000001-000020):
--   * 唯一性用 CREATE UNIQUE INDEX (不用内联 UNIQUE(...)WHERE, golang-migrate 内联
--     写法非法), 与 000018 idx_users_tenant_username / 000020 uniq_payments_out_trade_no
--     一致。
--   * 既有非唯一索引 idx_subauth_tenant_member 保留不动 (唯一索引是其超集, 二者并存
--     无害; 删旧索引非必需且降低回滚风险), 与 000020 保留旧表约束的保守做法一致。
--   * 合并满足 CHECK(used_quota <= granted_quota): 各行 used<=granted ⇒ SUM(used)<=SUM(granted)。
-- ============================================================

-- 1. 合并存量重复行: 把每组的配额汇总写回 keeper(MIN(id)) 行。
WITH grp AS (
    SELECT tenant_id, member_id, template_id,
           MIN(id)            AS keep_id,
           SUM(granted_quota) AS sum_granted,
           SUM(used_quota)    AS sum_used,
           MAX(granted_at)    AS last_granted
    FROM subscribe_auth
    GROUP BY tenant_id, member_id, template_id
    HAVING COUNT(*) > 1
)
UPDATE subscribe_auth s
SET granted_quota = grp.sum_granted,
    used_quota    = grp.sum_used,
    granted_at    = grp.last_granted
FROM grp
WHERE s.id = grp.keep_id;

-- 2. 删除已折叠进 keeper 的其余重复行 (组内非 MIN(id) 的行)。
DELETE FROM subscribe_auth s
USING (
    SELECT tenant_id, member_id, template_id, MIN(id) AS keep_id
    FROM subscribe_auth
    GROUP BY tenant_id, member_id, template_id
    HAVING COUNT(*) > 1
) grp
WHERE s.tenant_id   = grp.tenant_id
  AND s.member_id   = grp.member_id
  AND s.template_id = grp.template_id
  AND s.id         <> grp.keep_id;

-- 3. 建唯一索引, 作为 GrantSubscribe upsert 的 ON CONFLICT 目标。
CREATE UNIQUE INDEX IF NOT EXISTS uq_subauth_tenant_member_tpl
    ON subscribe_auth (tenant_id, member_id, template_id);

COMMENT ON INDEX uq_subauth_tenant_member_tpl IS '一(租户,会员,模板)一行授权; GrantSubscribe ON CONFLICT 累加 granted_quota 的冲突目标, 杜绝并发首授插重复行';
