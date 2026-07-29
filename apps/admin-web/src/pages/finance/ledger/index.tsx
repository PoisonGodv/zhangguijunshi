// src/pages/finance/ledger/index.tsx — 财务占位页 (FE-ADMIN-01)
// A2 后: 进件/支付单/账本流水 + 对账 (供应商/外卖平台差异) + 订阅付费墙 + 套餐功能校验。
// payment/subscription 无 json tag → PascalCase; 反二清 (payment 不持资, 资金直清 sub_merchant)。
import PlaceholderCard from '@/components/PlaceholderCard'

export default function LedgerPage() {
  return (
    <PlaceholderCard
      module="finance"
      title="财务 — 进件/账本/订阅墙待填充"
      description="特约商户进件/支付单/账本流水/订阅付费墙。SubMerchant/Payment/Ledger PascalCase; 进件未激活 60023。"
      endpoints={[
        { method: 'POST', path: '/v1/boss/payment/sub-merchants', desc: '微信支付服务商进件 (body: business_name/license_no...)' },
        { method: 'GET', path: '/v1/boss/payment/sub-merchants', desc: '进件记录列表' },
        { method: 'POST', path: '/v1/boss/payment/sub-merchants/:id/query', desc: '查询进件状态' },
        { method: 'POST', path: '/v1/boss/payment/prepay', desc: '预下单 (body: subject/amount_cents/openid...)' },
        { method: 'GET', path: '/v1/boss/payment/payments/:id', desc: '支付单详情' },
        { method: 'GET', path: '/v1/boss/payment/ledger', desc: '账本流水 ?limit=' },
        { method: 'GET', path: '/v1/boss/subscription', desc: '当前订阅' },
        { method: 'POST', path: '/v1/boss/subscription', desc: '订阅/升档 (body: plan/period_months...); 套餐不足 40303' },
        { method: 'POST', path: '/v1/boss/subscription/check', desc: '功能权限校验 (body: feature)' },
      ]}
    />
  )
}
