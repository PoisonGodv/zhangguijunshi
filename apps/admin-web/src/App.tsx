// src/App.tsx — 应用根 (FE-ADMIN-01)
// ConfigProvider(中文/暖色) + React Router v6 useRoutes + ErrorBoundary + BasicLayout 包裹
import { ConfigProvider, App as AntdApp } from 'antd'
import zhCN from 'antd/locale/zh_CN'
import { Navigate, useRoutes } from 'react-router-dom'
import ErrorBoundary from '@/components/ErrorBoundary'
import BasicLayout from '@/layouts/BasicLayout'
import DashboardPage from '@/pages/dashboard'
import ProductsPage from '@/pages/catalog/products'
import StockPage from '@/pages/inventory/stock'
import MembersPage from '@/pages/membership/members'
import CouponsPage from '@/pages/marketing/coupons'
import StrategiesPage from '@/pages/aiengine/strategies'
import LedgerPage from '@/pages/finance/ledger'
import SystemPage from '@/pages/system'
import LoginPage from '@/pages/login'
import NotFoundPage from '@/pages/404'

export default function App() {
  const routes = useRoutes([
    { path: '/login', element: <LoginPage /> },
    {
      path: '/',
      element: <BasicLayout />,
      children: [
        { index: true, element: <Navigate to="/dashboard" replace /> },
        { path: 'dashboard', element: <DashboardPage /> },
        { path: 'catalog/products', element: <ProductsPage /> },
        { path: 'inventory/stock', element: <StockPage /> },
        { path: 'membership/members', element: <MembersPage /> },
        { path: 'marketing/coupons', element: <CouponsPage /> },
        { path: 'aiengine/strategies', element: <StrategiesPage /> },
        { path: 'finance/ledger', element: <LedgerPage /> },
        { path: 'system', element: <SystemPage /> },
        { path: '*', element: <NotFoundPage /> },
      ],
    },
  ])

  return (
    <ConfigProvider
      locale={zhCN}
      theme={{
        token: {
          colorPrimary: '#FF6B35', // 零售暖色 (与 merchant-mp tab 主色一致)
        },
      }}
    >
      <AntdApp>
        <ErrorBoundary>{routes}</ErrorBoundary>
      </AntdApp>
    </ConfigProvider>
  )
}
