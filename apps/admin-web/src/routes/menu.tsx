// src/routes/menu.tsx — 侧边栏菜单 + 选中态 (FE-ADMIN-01 前端硬编码)
// BE-A-01 落地后: 菜单改由 /v1/admin/me 返回 permissions.menus 下发, 前端按 RBAC 过滤。
// 路由表见 src/App.tsx (useRoutes); 此处仅维护菜单元数据。
import type { ReactNode } from 'react'
import type { MenuProps } from 'antd'
import { useLocation } from 'react-router-dom'
import {
  DashboardOutlined,
  ShopOutlined,
  InboxOutlined,
  TeamOutlined,
  GiftOutlined,
  RobotOutlined,
  WalletOutlined,
  SettingOutlined,
} from '@ant-design/icons'

export interface MenuRoute {
  path: string
  name: string
  icon?: ReactNode
}

// 菜单项 (与 App.tsx 路由对齐)
export const MENU_ROUTES: MenuRoute[] = [
  { path: '/dashboard', name: '经营看板', icon: <DashboardOutlined /> },
  { path: '/catalog/products', name: '商品管理', icon: <ShopOutlined /> },
  { path: '/inventory/stock', name: '库存管理', icon: <InboxOutlined /> },
  { path: '/membership/members', name: '会员管理', icon: <TeamOutlined /> },
  { path: '/marketing/coupons', name: '营销中心', icon: <GiftOutlined /> },
  { path: '/aiengine/strategies', name: 'AI 策略', icon: <RobotOutlined /> },
  { path: '/finance/ledger', name: '财务', icon: <WalletOutlined /> },
  { path: '/system', name: '系统设置', icon: <SettingOutlined /> },
]

// antd Menu items (key = path)
export type MenuItem = Required<MenuProps>['items'][number]
export const MENU_ITEMS: MenuItem[] = MENU_ROUTES.map((r) => ({
  key: r.path,
  icon: r.icon,
  label: r.name,
}))

// 选中态高亮: 取最长前缀匹配的菜单项 (ProLayout selectedKeys)
export function useSelectedKeys(): string[] {
  const loc = useLocation()
  const matched = MENU_ROUTES
    .filter((r) => loc.pathname.startsWith(r.path))
    .sort((a, b) => b.path.length - a.path.length)[0]
  return matched ? [matched.path] : ['/dashboard']
}
