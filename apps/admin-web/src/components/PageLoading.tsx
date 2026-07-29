// src/components/PageLoading.tsx — 全屏/区块加载占位
import { Spin } from 'antd'

export default function PageLoading({ tip = '加载中...' }: { tip?: string }) {
  return (
    <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', height: '60vh', width: '100%' }}>
      <Spin size="large" tip={tip} />
    </div>
  )
}
