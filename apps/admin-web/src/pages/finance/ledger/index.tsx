// src/pages/finance/ledger/index.tsx — 财务占位页 (FE-ADMIN-01)
// MVP 免费版 (CONTRACT §25.2): 在线支付与订阅付费墙因微信支付服务商资质审核未过,
// 已在配置层整体下架 (enabled=false → 路由不注册 404)。payment/subscription 代码保留待用,
// V1 完成资质审核后重新开启即可恢复进件/支付单/账本流水/对账/订阅付费墙等能力。
// 反二清设计 (payment 不持资, 资金直清 sub_merchant) 不变。
import PlaceholderCard from '@/components/PlaceholderCard'

export default function LedgerPage() {
  return (
    <PlaceholderCard
      module="finance"
      title="财务 — 暂未启用 (MVP 免费版)"
      description="MVP 阶段为免费版, 在线支付与订阅付费墙未启用: 微信支付服务商资质审核未过, 进件/预下单/支付单/账本流水/订阅相关接口已在配置层下架 (enabled=false → 路由不注册, 访问返回 404)。相关后端代码与反二清设计均保留待用, V1 完成资质审核后重新开启即可恢复, 无需改动本页。"
      endpoints={[]}
    />
  )
}
