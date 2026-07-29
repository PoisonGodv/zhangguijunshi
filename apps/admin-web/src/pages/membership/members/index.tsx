// src/pages/membership/members/index.tsx — 会员管理 (FE-ADMIN-02)
//
// ProTable 分页会员列表 (后端真分页 page/size) + 画像 Drawer (详情 + 储值充值 +
// 会员码下发/核销)。memberDTO 为 snake_case (handler 内联结构体带 json tag)。
//
// 接口:
//   GET  /v1/boss/membership/members?page=&size=&level=&q= → listMembers (真分页)
//   GET  /v1/boss/membership/members/:id                   → getMember (画像详情)
//   POST /v1/boss/membership/members/:id/recharge          → recharge (储值充值)
//   POST /v1/boss/membership/redeem                        → redeem (会员码核销)
//   POST /v1/boss/membership/members/:id/code              → issueCode (下发动态码)

import { useRef, useState } from 'react'
import { Button, Tag, Space, Drawer, Descriptions, App } from 'antd'
import { ReloadOutlined, ProfileOutlined, GiftOutlined, QrcodeOutlined } from '@ant-design/icons'
import type { ActionType, ProColumns } from '@ant-design/pro-components'
import { ProTable, ModalForm, ProForm, ProFormDigit, ProFormText } from '@ant-design/pro-components'
import Money from '@/components/Money'
import { fmtDateTime } from '@/utils/format'
import {
  listMembers,
  getMember,
  recharge,
  redeem,
  issueCode,
  type MemberDTO,
} from '@/services/membership'

const LEVEL_TAG: Record<string, { color: string; text: string }> = {
  normal: { color: 'default', text: '普通会员' },
  vip: { color: 'blue', text: '银卡会员' },
  blackcard: { color: 'gold', text: '金卡会员' },
}

export default function MembersPage() {
  const { message: msg } = App.useApp()
  const actionRef = useRef<ActionType>(null)
  const [profileRow, setProfileRow] = useState<MemberDTO | null>(null)
  const [profileOpen, setProfileOpen] = useState(false)
  const [profileDetail, setProfileDetail] = useState<MemberDTO | null>(null)

  // 充值 ModalForm
  const [rechargeOpen, setRechargeOpen] = useState(false)
  const [rechargeForm] = ProForm.useForm()
  // 核销 ModalForm
  const [redeemOpen, setRedeemOpen] = useState(false)
  const [redeemForm] = ProForm.useForm()
  // 下发的码 (展示用)
  const [issuedCode, setIssuedCode] = useState<string | null>(null)

  const openProfile = async (row: MemberDTO) => {
    setProfileRow(row)
    setProfileDetail(row)
    setProfileOpen(true)
    setIssuedCode(null)
    // 拉最新详情兜底 (余额/积分可能滞后)
    try {
      const fresh = await getMember(row.id)
      setProfileDetail(fresh)
    } catch {
      // 用列表行兜底
    }
  }

  const handleRecharge = async (vals: Record<string, unknown>) => {
    if (!profileRow) return false
    const amount = Math.round(Number(vals.amount_cents ?? 0) * 100)
    if (!amount || amount <= 0) {
      msg.error('充值金额必须 > 0')
      return false
    }
    const res = await recharge(profileRow.id, { amount_cents: amount })
    msg.success(`充值成功: 本次 ${res.RechargedCents / 100} 元${res.GiftedCents ? ` (赠送 ${res.GiftedCents / 100} 元)` : ''}, 余额 ${res.BalanceCents / 100} 元`)
    setRechargeOpen(false)
    rechargeForm.resetFields()
    // 刷新画像 + 列表
    try {
      const fresh = await getMember(profileRow.id)
      setProfileDetail(fresh)
    } catch { /* ignore */ }
    actionRef.current?.reload()
    return true
  }

  const handleRedeem = async (vals: Record<string, unknown>) => {
    const code = (vals.code as string | undefined)?.trim()
    if (!code) {
      msg.error('请输入会员码')
      return false
    }
    const consume = vals.consume_cents ? Math.round(Number(vals.consume_cents) * 100) : undefined
    const earn = vals.earn_points ? Number(vals.earn_points) : undefined
    if (!consume && !earn) {
      msg.error('请填扣减金额 或 累积积分 (至少一项)')
      return false
    }
    const res = await redeem({
      code,
      consume_cents: consume,
      earn_points: earn,
    })
    msg.success(`核销成功: 会员 ${res.MemberID}, 积分 ${res.Points}, 余额 ${res.BalanceCents / 100} 元`)
    setRedeemOpen(false)
    redeemForm.resetFields()
    actionRef.current?.reload()
    return true
  }

  const handleIssueCode = async () => {
    if (!profileRow) return
    try {
      const res = await issueCode(profileRow.id)
      setIssuedCode(res.code)
      msg.success('会员码已下发 (有效期 0.5s, 仅供核销流转)')
    } catch { /* ApiError 已提示 */ }
  }

  const columns: ProColumns<MemberDTO>[] = [
    { title: 'ID', dataIndex: 'id', width: 64, hideInSearch: true },
    {
      title: '姓名',
      dataIndex: 'name',
      width: 120,
      hideInSearch: true,
      render: (_, r) => r.name || <Tag>未填</Tag>,
    },
    {
      title: '等级',
      dataIndex: 'level',
      width: 110,
      valueType: 'select',
      valueEnum: {
        normal: { text: '普通会员' },
        vip: { text: '银卡会员' },
        blackcard: { text: '金卡会员' },
      },
      render: (_, r) => {
        const t = LEVEL_TAG[r.level] || { color: 'default', text: r.level }
        return <Tag color={t.color}>{t.text}</Tag>
      },
    },
    { title: '积分', dataIndex: 'points', width: 90, hideInSearch: true },
    {
      title: '储值余额',
      dataIndex: 'balance_cents',
      width: 120,
      hideInSearch: true,
      render: (_, r) => <Money value={r.balance_cents} />,
    },
    {
      title: '累计消费',
      dataIndex: 'total_spent_cents',
      width: 120,
      hideInSearch: true,
      render: (_, r) => <Money value={r.total_spent_cents} colorSign={false} />,
    },
    {
      title: '状态',
      dataIndex: 'block',
      width: 90,
      hideInSearch: true,
      render: (_, r) => (r.block ? <Tag color="red">已冻结</Tag> : <Tag color="green">正常</Tag>),
    },
    {
      title: '注册时间',
      dataIndex: 'created_at',
      width: 170,
      hideInSearch: true,
      render: (_, r) => fmtDateTime(r.created_at),
    },
    {
      title: '关键词',
      dataIndex: 'q',
      hideInTable: true,
      fieldProps: { placeholder: '姓名/手机号 模糊' },
    },
    {
      title: '操作',
      key: 'actions',
      width: 110,
      fixed: 'right',
      hideInSearch: true,
      render: (_, row) => (
        <Button type="link" size="small" icon={<ProfileOutlined />} onClick={() => openProfile(row)}>
          画像
        </Button>
      ),
    },
  ]

  return (
    <>
      <ProTable<MemberDTO>
        headerTitle="会员管理"
        actionRef={actionRef}
        rowKey="id"
        columns={columns}
        scroll={{ x: 1100 }}
        search={{ labelWidth: 80, defaultCollapsed: false }}
        options={{ density: false, reload: true }}
        request={async (params) => {
          // 后端真分页: page=current, size=pageSize
          const page = params.current ?? 1
          const size = params.pageSize ?? 10
          const level = params.level as string | undefined
          const q = (params.q as string | undefined)?.trim() || undefined
          const resp = await listMembers({ page, size, level, q })
          return {
            data: resp.items || [],
            success: true,
            total: resp.total ?? 0,
          }
        }}
        toolBarRender={() => [
          <Button
            key="reload"
            icon={<ReloadOutlined />}
            onClick={() => actionRef.current?.reload()}
          >
            刷新
          </Button>,
        ]}
      />

      {/* 画像抽屉 */}
      <Drawer
        title="会员画像"
        width={520}
        open={profileOpen}
        onClose={() => setProfileOpen(false)}
        destroyOnClose
        extra={
          <Space>
            <Button icon={<QrcodeOutlined />} onClick={handleIssueCode} disabled={!profileRow}>
              下发会员码
            </Button>
            <Button
              type="primary"
              icon={<GiftOutlined />}
              onClick={() => {
                rechargeForm.resetFields()
                setRechargeOpen(true)
              }}
              disabled={!profileRow || profileRow?.block}
            >
              储值充值
            </Button>
            <Button
              danger
              onClick={() => {
                redeemForm.resetFields()
                setRedeemOpen(true)
              }}
            >
              核销会员码
            </Button>
          </Space>
        }
      >
        {profileDetail && (
          <>
            {profileDetail.block && (
              <Tag color="red" style={{ marginBottom: 16 }}>该会员已冻结 (42302)</Tag>
            )}
            {issuedCode && (
              <Tag color="blue" style={{ marginBottom: 16, fontSize: 14, padding: '4px 12px' }}>
                动态码: {issuedCode}
              </Tag>
            )}
            <Descriptions column={1} bordered size="small">
              <Descriptions.Item label="会员 ID">{profileDetail.id}</Descriptions.Item>
              <Descriptions.Item label="姓名">{profileDetail.name || '未填'}</Descriptions.Item>
              <Descriptions.Item label="等级">
                <Tag color={LEVEL_TAG[profileDetail.level]?.color || 'default'}>
                  {profileDetail.level_name || profileDetail.level}
                </Tag>
              </Descriptions.Item>
              <Descriptions.Item label="积分">{profileDetail.points}</Descriptions.Item>
              <Descriptions.Item label="储值余额">
                <Money value={profileDetail.balance_cents} />
              </Descriptions.Item>
              <Descriptions.Item label="累计消费">
                <Money value={profileDetail.total_spent_cents} colorSign={false} />
              </Descriptions.Item>
              <Descriptions.Item label="注册时间">{fmtDateTime(profileDetail.created_at)}</Descriptions.Item>
              <Descriptions.Item label="状态">
                {profileDetail.block ? <Tag color="red">已冻结</Tag> : <Tag color="green">正常</Tag>}
              </Descriptions.Item>
            </Descriptions>
            <div style={{ marginTop: 16, color: '#999', fontSize: 12 }}>
              储值充值资金由微信支付直清子商户 (反二清), 此处仅记账; 地区单卡上限
              触发 errcode 60003, 余额不足 60004。
            </div>
          </>
        )}
      </Drawer>

      {/* 储值充值 ModalForm */}
      <ModalForm
        title={`储值充值 — 会员 ${profileRow?.id ?? ''}`}
        open={rechargeOpen}
        onOpenChange={setRechargeOpen}
        form={rechargeForm}
        modalProps={{ destroyOnClose: true, maskClosable: false }}
        width={440}
        onFinish={handleRecharge}
        submitter={{ searchConfig: { submitText: '充值', resetText: '取消' } }}
      >
        <div style={{ marginBottom: 16 }}>
          <Tag color="orange">充 200 送 20</Tag>
          <Tag color="blue">充 500 送 80</Tag>
          <Tag color="gold">充 1000 送 200</Tag>
        </div>
        <ProFormDigit
          name="amount_cents"
          label="充值金额 (元)"
          min={0.01}
          precision={2}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0.00"
          rules={[{ required: true, message: '请输入充值金额' }]}
          extra={profileDetail ? `当前余额 ${profileDetail.balance_cents / 100} 元` : undefined}
        />
      </ModalForm>

      {/* 核销会员码 ModalForm */}
      <ModalForm
        title="核销会员码"
        open={redeemOpen}
        onOpenChange={setRedeemOpen}
        form={redeemForm}
        modalProps={{ destroyOnClose: true, maskClosable: false }}
        width={440}
        onFinish={handleRedeem}
        submitter={{ searchConfig: { submitText: '核销', resetText: '取消' } }}
      >
        <div style={{ marginBottom: 16 }}>
          <Tag color="red">扣减储值 或 累积积分, 至少填一项</Tag>
          <Tag color="orange">码失效 40003 / 余额不足 60004 / 冻结 42302</Tag>
        </div>
        <ProFormText
          name="code"
          label="会员码"
          placeholder="会员动态码"
          rules={[{ required: true, message: '请输入会员码' }]}
        />
        <ProFormDigit
          name="consume_cents"
          label="扣减储值 (元)"
          min={0}
          precision={2}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0.00 (可选)"
        />
        <ProFormDigit
          name="earn_points"
          label="累积积分"
          min={0}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0 (可选)"
        />
      </ModalForm>
    </>
  )
}
