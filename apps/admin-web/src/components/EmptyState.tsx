// src/components/EmptyState.tsx — 空状态占位 (用于占位页/未实现模块)
import { Empty, Typography } from 'antd'
import type { ReactNode } from 'react'

const { Title, Paragraph } = Typography

interface Props {
  title?: string
  description?: string
  extra?: ReactNode
}

export default function EmptyState({
  title = '模块建设中',
  description = '该模块接口已就绪, 待 A2 填充 ProTable/表单',
  extra,
}: Props) {
  return (
    <div style={{ padding: '24px 0' }}>
      <Empty description={false} />
      <Title level={4} style={{ marginTop: 16, marginBottom: 8 }}>
        {title}
      </Title>
      <Paragraph type="secondary">{description}</Paragraph>
      {extra}
    </div>
  )
}
