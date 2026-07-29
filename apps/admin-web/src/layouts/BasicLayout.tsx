// src/layouts/BasicLayout.tsx — ProLayout 侧边栏 + Outlet (FE-ADMIN-01)
// 菜单前端硬编码; BE-A-01 后由 /v1/admin/me 下发 permissions.menus。
import { ProLayout, type MenuDataItem } from '@ant-design/pro-components'
import { useNavigate, Outlet, useLocation } from 'react-router-dom'
import { Tooltip, Tag } from 'antd'
import { MENU_ROUTES, useSelectedKeys } from '@/routes/menu'

export default function BasicLayout() {
  const navigate = useNavigate()
  const location = useLocation()
  const selectedKeys = useSelectedKeys()

  // menuData: ProLayout 侧边栏数据 (key=path → 点击 navigate)
  const menuData: MenuDataItem[] = MENU_ROUTES.map((r) => ({
    path: r.path,
    name: r.name,
    icon: r.icon as React.ReactNode,
  }))

  return (
    <ProLayout
      title="掌柜军师"
      logo={null}
      layout="mix"
      fixedHeader
      fixSiderbar
      location={{ pathname: location.pathname }}
      selectedKeys={selectedKeys}
      menu={{ request: async () => menuData }}
      menuItemRender={(item, dom) => (
        <div
          onClick={() => item.path && navigate(item.path)}
          style={{ cursor: 'pointer' }}
        >
          {dom}
        </div>
      )}
      avatarProps={{
        title: 'dev',
        size: 'small',
        render: (_props, dom) => (
          <Tooltip title="dev 头模式 (X-Mock-Tenant:1)。BE-A-01 后改 JWT。">
            {dom}
          </Tooltip>
        ),
      }}
      footerRender={() => (
        <div style={{ textAlign: 'center', color: '#999', fontSize: 12 }}>
          掌柜军师管理后台 · FE-ADMIN-01 骨架 · dev 头模式{' '}
          <Tag color="orange">tenant=1</Tag>
        </div>
      )}
    >
      <Outlet />
    </ProLayout>
  )
}
