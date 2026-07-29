// src/components/PlaceholderCard.tsx — 占位页统一组件 (FE-ADMIN-01)
// 列出该模块将调用的 boss 接口路径, A2 agent 后续替换为真实 ProTable/表单。
import { Card, Typography, Tag, Space } from 'antd'
import EmptyState from './EmptyState'

const { Title, Text } = Typography

interface Endpoint {
  method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  path: string
  desc?: string
}

interface Props {
  module: string
  title: string
  description?: string
  endpoints: Endpoint[]
}

const METHOD_COLOR: Record<Endpoint['method'], string> = {
  GET: 'blue',
  POST: 'green',
  PUT: 'orange',
  PATCH: 'gold',
  DELETE: 'red',
}

export default function PlaceholderCard({ module, title, description, endpoints }: Props) {
  return (
    <Card>
      <EmptyState
        title={title}
        description={description ?? `模块建设中 — ${module} 域接口已就绪, 待 A2 填充 ProTable/表单`}
      />
      <Title level={5} style={{ marginTop: 8, marginBottom: 12 }}>
        接口已就绪 (BE-A-01 后菜单由 /v1/admin/me 下发)
      </Title>
      <Space direction="vertical" size="small" style={{ width: '100%' }}>
        {endpoints.map((ep) => (
          <div key={ep.method + ep.path} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <Tag color={METHOD_COLOR[ep.method]} style={{ margin: 0, width: 56, textAlign: 'center' }}>
              {ep.method}
            </Tag>
            <Text code copyable style={{ flex: 1 }}>
              {ep.path}
            </Text>
            {ep.desc && <Text type="secondary">{ep.desc}</Text>}
          </div>
        ))}
      </Space>
    </Card>
  )
}
