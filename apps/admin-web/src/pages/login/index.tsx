// src/pages/login/index.tsx — 商家登录页 (BE-A-01 真登录, scene=boss)
//
// 业务页全部调 /v1/boss/* (bossAdminAuthMW 要 scene=boss), 故登录必须走商家认证
// 拿 scene=boss 的 JWT, 否则 prod 下登录后业务页全 40101 (scene 失配)。
// 租户ID + 手机号 + 密码 → POST /v1/boss/auth/login → 存 admin_token +
// admin_refresh_token (localStorage) → 跳经营看板。request.ts 的请求拦截器随后
// 自动注入 Authorization: Bearer <jwt>。登录失败由 request 拦截器统一中文提示。
// (tenant_id 在登录时无鉴权身份, 只能由客户端提供; 之后所有 boss 请求 tenant 取自 JWT。)
import { useState } from 'react'
import { Form, Input, InputNumber, Button, Card, Typography, Alert, App } from 'antd'
import { UserOutlined, LockOutlined, ShopOutlined } from '@ant-design/icons'
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
  tenant_id: number
  phone: string
  password: string
}

export default function LoginPage() {
  const navigate = useNavigate()
  const { message } = App.useApp()
  const [loading, setLoading] = useState(false)

  const onFinish = async (values: LoginForm) => {
    setLoading(true)
    try {
      const res = await api.post<LoginResult>('/boss/auth/login', {
        tenant_id: values.tenant_id,
        phone: values.phone.trim(),
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
          掌柜军师 · 商家后台
        </Title>
        <Paragraph type="secondary" style={{ textAlign: 'center', marginBottom: 24 }}>
          请登录商家管理后台
        </Paragraph>
        <Form<LoginForm> layout="vertical" onFinish={onFinish} autoComplete="off">
          <Form.Item
            name="tenant_id"
            label="租户ID"
            rules={[{ required: true, message: '请输入租户ID (整数 ≥1)' }]}
          >
            <InputNumber
              prefix={<ShopOutlined />}
              placeholder="租户ID"
              size="large"
              style={{ width: '100%' }}
              min={1}
              precision={0}
            />
          </Form.Item>
          <Form.Item
            name="phone"
            label="手机号"
            rules={[{ required: true, message: '请输入手机号' }]}
          >
            <Input prefix={<UserOutlined />} placeholder="手机号" size="large" />
          </Form.Item>
          <Form.Item
            name="password"
            label="密码"
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
            description="未登录时请求会回落 X-Mock-* 头直连 (request.ts)。demo 商家账号: 租户ID=1 / 手机号=13800000001 / 密码=boss12345 (seed 样板店)。"
            style={{ marginTop: 8 }}
          />
        )}
      </Card>
    </div>
  )
}
