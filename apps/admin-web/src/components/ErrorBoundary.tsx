// src/components/ErrorBoundary.tsx — React 错误边界 (class 组件, antd Result 兜底)
import React from 'react'
import { Result, Button } from 'antd'

interface Props {
  children: React.ReactNode
}
interface State {
  hasError: boolean
  message: string
}

export default class ErrorBoundary extends React.Component<Props, State> {
  constructor(props: Props) {
    super(props)
    this.state = { hasError: false, message: '' }
  }

  static getDerivedStateFromError(err: Error): State {
    return { hasError: true, message: err.message || '页面渲染异常' }
  }

  componentDidCatch(err: Error, info: React.ErrorInfo): void {
    // 预留: 接入埋点上报
    console.error('[ErrorBoundary]', err, info)
  }

  handleReset = (): void => {
    this.setState({ hasError: false, message: '' })
  }

  render(): React.ReactNode {
    if (this.state.hasError) {
      return (
        <Result
          status="error"
          title="页面渲染异常"
          subTitle={this.state.message}
          extra={
            <Button type="primary" onClick={this.handleReset}>
              重试
            </Button>
          }
        />
      )
    }
    return this.props.children
  }
}
