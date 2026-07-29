// src/components/Money.tsx — 分 → ¥ 格式化展示组件 (复用 consumer-mp format 思路)
import { yuanText } from '@/utils/format'

interface Props {
  /** 金额, 单位分 (后端金额统一分) */
  value: number | string | null | undefined
  /** 负数红色, 默认 true */
  colorSign?: boolean
  style?: React.CSSProperties
}

export default function Money({ value, colorSign = true, style }: Props) {
  const cents = typeof value === 'string' ? parseInt(value, 10) : value ?? 0
  const text = yuanText(cents)
  const isNeg = cents < 0
  const color = colorSign && isNeg ? '#cf1322' : undefined
  return <span style={{ fontVariantNumeric: 'tabular-nums', color, ...style }}>{text}</span>
}
