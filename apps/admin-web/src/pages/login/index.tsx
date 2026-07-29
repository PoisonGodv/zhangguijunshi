// src/pages/login/index.tsx — 登录页占位 (FE-ADMIN-01)
// 本轮不实现真登录 (BE-A-01 延后); 重定向到 dashboard 并提示 dev 头模式。
import { useEffect } from 'react'
import { Result, Button, Typography, Alert } from 'antd'
import { useNavigate } from 'react-router-dom'

const { Paragraph } = Typography

export default function LoginPage() {
  const navigate = useNavigate()
  useEffect(() => {
    // 本轮 dev 头模式: 不校验 token, 直接重定向看板。
    // BE-A-01 落地后: 在此实现账号密码登录 → /v1/admin/login → 存 admin_token。
    const t = setTimeout(() => navigate('/dashboard', { replace: true }), 0)
    return () => clearTimeout(t)
  }, [navigate])

  return (
    <Result
      status="info"
      title="管理后台登录 (待 BE-A-01 实现)"
      subTitle="当前为 dev 头直连模式, 无需登录, 已自动重定向至经营看板。"
      extra={
        <div>
          <Alert
            type="warning"
            showIcon
            message="dev 模式: 请求注入 X-Mock-Tenant=1 / X-Mock-Store=0 / X-Mock-User=dev / X-Mock-Role=owner"
            description="prod 模式改为 JWT Authorization 头 (BE-A-01 落地后启用)。"
            style={{ marginBottom: 16, textAlign: 'left' }}
          />
          <Paragraph type="secondary">
            鉴权头注入逻辑见 <code>src/services/request.ts</code>。
          </Paragraph>
          <Button type="primary" onClick={() => navigate('/dashboard', { replace: true })}>
            进入经营看板
          </Button>
        </div>
      }
    />
  )
}
