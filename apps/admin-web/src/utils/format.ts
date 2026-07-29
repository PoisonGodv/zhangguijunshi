// src/utils/format.ts — 展示格式化纯函数 (零依赖, 复用 consumer-mp format 思路)

/** 分 → 元字符串: 整数 ¥10, 否则两位小数 ¥10.50。接受 number|string。 */
export function yuan(cents: number | string): string {
  const n = typeof cents === 'string' ? parseFloat(cents) : cents
  if (isNaN(n)) return '0'
  const yuanVal = n / 100
  return yuanVal % 1 === 0 ? String(yuanVal) : yuanVal.toFixed(2)
}

/** 分 → ¥ 带符号格式化: ¥10 / ¥10.50 */
export function yuanText(cents: number | string): string {
  return '¥' + yuan(cents)
}

/** 手机号脱敏: 138****1234 */
export function maskPhone(phone: string): string {
  if (!phone) return ''
  if (phone.length === 11) return phone.slice(0, 3) + '****' + phone.slice(7)
  if (phone.length > 4) return phone.slice(0, 3) + '****' + phone.slice(-4)
  return phone
}

function pad2(n: number): string {
  return n < 10 ? '0' + n : String(n)
}

/** ISO 时间 → YYYY-MM-DD */
export function fmtDate(iso: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d.getTime())) return ''
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`
}

/** ISO 时间 → YYYY-MM-DD HH:mm:ss */
export function fmtDateTime(iso: string): string {
  if (!iso) return ''
  const d = new Date(iso)
  if (isNaN(d.getTime())) return ''
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ` +
    `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`
}
