// src/pages/system/index.tsx — 系统设置占位页 (FE-ADMIN-01)
// A2 后: RBAC 角色/后台账号/审计日志/多租户管理 (总部视角)
import PlaceholderCard from '@/components/PlaceholderCard'

export default function SystemPage() {
  return (
    <PlaceholderCard
      module="system"
      title="系统设置 — RBAC/账号/审计日志待填充"
      description="BE-A-01 后: RBAC 角色权限 / 后台账号 / 审计日志 / 多租户管理 (总部跨租户)。本轮纯占位。"
      endpoints={[
        { method: 'GET', path: '/v1/admin/me', desc: 'BE-A-01 后: 当前用户 + 菜单 + 权限 (RBAC)' },
        { method: 'GET', path: '/v1/boss/whoami', desc: '当前租户身份 (dev 头模式)' },
      ]}
    />
  )
}
