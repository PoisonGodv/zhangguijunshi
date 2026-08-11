// src/pages/login/index.tsx — 平台运营登录页 (FE-ADMIN / BE-A-01 真登录)
//
// 账号(平台运营 name)+密码登录 → POST /v1/admin/auth/login → 存 admin_token +
// admin_refresh_token (localStorage) → 跳经营看板。request.ts 的请求拦截器随后
// 自动注入 Authorization: Bearer <jwt>。登录失败由 request 拦截器统一中文提示。
import { useState } from 'react'
import { Form, Input, Button, Card, Typography, Alert, App } from 'antd'
import { UserOutlined, LockOutlined } from '@ant-design/icons'
import { useNavigate } from 'react-router-dom'
import { api, setToken, setRefreshToken } from '@/services/request'

const { Title, Paragraph } = Typography

interface LoginResult {
  access_token: string
  refresh_token: string
  expires_in: number
  token_type: string
}

interface LoginForm {
  username: string
  password: string
}

export default function LoginPage() {
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [loading, setLoading] = useState(false)

  const onFinish = async (values: LoginForm) => {
    setLoading(true)
    try {
      const res = await api.post<LoginResult>('/admin/auth/login', {
        username: values.username.trim(),
        password: values.password,
      })
      if (!res?.access_token) {
        message.error('登录响应缺少 token')
        return
      }
      setToken(res.access_token)
      if (res.refresh_token) {
        // 401 换签所需 (request.ts tryRefreshAndRetry); 不存则过期即强制重登。
        setRefreshToken(res.refresh_token)
      }
      message.success('登录成功')
      navigate('/dashboard', { replace: true })
    } catch {
      // 错误提示已由 request 拦截器统一处理 (中文映射)。
    } finally {
      setLoading(false)
    }
  }

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        background: '#f0f2f5',
      }}
    >
      <Card style={{ width: 380, boxShadow: '0 2px 8px rgba(0,0,0,0.09)' }}>
        <Title level={3} style={{ textAlign: 'center', marginBottom: 4 }}>
          掌柜军师 · 平台运营
        </Title>
        <Paragraph type="secondary" style={{ textAlign: 'center', marginBottom: 24 }}>
          请登录管理后台
        </Paragraph>
        <Form<LoginForm> layout="vertical" onFinish={onFinish} autoComplete="off">
          <Form.Item
            name="username"
            rules={[{ required: true, message: '请输入账号' }]}
          >
            <Input prefix={<UserOutlined />} placeholder="账号 (平台运营 name)" size="large" />
          </Form.Item>
          <Form.Item
            name="password"
            rules={[{ required: true, message: '请输入密码' }]}
          >
            <Input.Password prefix={<LockOutlined />} placeholder="密码" size="large" />
          </Form.Item>
          <Form.Item>
            <Button type="primary" htmlType="submit" size="large" block loading={loading}>
              登录
            </Button>
          </Form.Item>
        </Form>
        {import.meta.env.DEV && (
          <Alert
            type="info"
            showIcon
            message="dev 提示"
            description="未登录时请求会回落 X-Mock-* 头直连 (request.ts)。demo 平台账号: name=ops / 密码=ops12345 (tenant 1)。"
            style={{ marginTop: 8 }}
          />
        )}
      </Card>
    </div>
  )
}
