// src/pages/marketing/coupons/index.tsx — 营销券 (FE-ADMIN-02)
//
// ProTable 券模板列表 (Coupon 外层 PascalCase; CouponParams 嵌套 snake_case)。
// 列: ID/Name/Type Tag/Value(满threshold减off | discount_permille‰→折)/
//     ValidFrom~ValidTo/Status Tag/Issued/TotalQuota。
// 工具栏"新建券"→ ModalForm (Name/Type/ThresholdCents/OffCents/DiscountPermille/
//   ValidFrom/ValidTo/TotalQuota) 调 createCoupon (snake_case body)。
// 行操作: "上线/下线" 调 updateStatus(PATCH); "发券" → ModalForm
//   (member_id + coupon_id) 调 issue, 60001 毛利护栏由 request.ts errcode 映射 toast。

import { useCallback, useRef, useState } from 'react'
import { Button, Space, Tag, Popconfirm, Modal, Form, InputNumber, Input, Select, DatePicker, Typography } from 'antd'
import { ProTable, type ActionType, type ProColumns } from '@ant-design/pro-components'
import dayjs from 'dayjs'
import {
  listCoupons,
  createCoupon,
  updateStatus,
  issueCoupon,
  type Coupon,
} from '@/services/marketing'
import { yuanText, fmtDate } from '@/utils/format'

const { Text } = Typography

// 券类型 → 中文 + 颜色
const TYPE_META: Record<string, { label: string; color: string }> = {
  full_reduction: { label: '满减', color: 'orange' },
  discount: { label: '折扣', color: 'blue' },
  cash: { label: '代金', color: 'green' },
  gift: { label: '赠品', color: 'purple' },
}

// 券状态 → 中文 + 颜色
const STATUS_META: Record<string, { label: string; color: string }> = {
  draft: { label: '草稿', color: 'default' },
  active: { label: '已上线', color: 'green' },
  paused: { label: '已暂停', color: 'gold' },
  ended: { label: '已终止', color: 'red' },
}

/** 券面值展示: 按 Type 渲染不同表达 */
function renderCouponValue(c: Coupon): string {
  switch (c.Type) {
    case 'full_reduction':
      return `满 ${yuanText(c.Params.threshold_cents ?? 0)} 减 ${yuanText(c.Params.off_cents ?? 0)}`
    case 'discount': {
      const permille = c.Params.discount_permille ?? 0
      // 850‰ = 8.5折
      const zhe = (permille / 100).toFixed(1)
      return `${zhe}折`
    }
    case 'cash':
      return `代金 ${yuanText(c.Params.off_cents ?? 0)}`
    case 'gift':
      return `赠品 #${c.Params.gift_product_id ?? 0}`
    default:
      return '-'
  }
}

export default function CouponsPage() {
  const actionRef = useRef<ActionType>(undefined)
  const [createOpen, setCreateOpen] = useState(false)
  const [issueState, setIssueState] = useState<{ open: boolean; coupon?: Coupon }>({
    open: false,
  })
  const [createForm] = Form.useForm()
  const [issueForm] = Form.useForm()
  const [submitting, setSubmitting] = useState(false)

  const reload = useCallback(() => {
    actionRef.current?.reload()
  }, [actionRef])

  // 新建券提交 (snake_case body)
  const handleCreate = useCallback(async () => {
    const values = await createForm.validateFields()
    setSubmitting(true)
    try {
      await createCoupon({
        name: values.name as string,
        type: values.type as string,
        params: {
          threshold_cents: values.threshold_cents as number | undefined,
          off_cents: values.off_cents as number | undefined,
          discount_permille: values.discount_permille as number | undefined,
          gift_product_id: values.gift_product_id as number | undefined,
        },
        valid_from: values.valid_from ? (values.valid_from as dayjs.Dayjs).toISOString() : undefined,
        valid_to: (values.valid_to as dayjs.Dayjs).toISOString(),
        total_quota: (values.total_quota as number) ?? 0,
      })
      setCreateOpen(false)
      createForm.resetFields()
      reload()
    } catch {
      // 校验失败或 request.ts 已 toast
    } finally {
      setSubmitting(false)
    }
  }, [createForm, reload])

  // 改状态 (上线/下线/暂停/终止)
  const handleUpdateStatus = useCallback(async (id: number, status: string) => {
    try {
      await updateStatus(id, status)
      reload()
    } catch {
      // request.ts 已 toast
    }
  }, [reload])

  // 发券提交 (member_id + coupon_id; 60001 毛利护栏由 request.ts 映射)
  const handleIssue = useCallback(async () => {
    if (!issueState.coupon) return
    const values = await issueForm.validateFields()
    setSubmitting(true)
    try {
      await issueCoupon({
        member_id: values.member_id as number,
        coupon_id: issueState.coupon.ID,
        source: 'manual',
      })
      setIssueState({ open: false })
      issueForm.resetFields()
    } catch {
      // request.ts 已 toast (含 60001 毛利护栏)
    } finally {
      setSubmitting(false)
    }
  }, [issueState, issueForm])

  const columns: ProColumns<Coupon>[] = [
    { title: 'ID', dataIndex: 'ID', width: 70, search: false },
    { title: '券名称', dataIndex: 'Name', width: 160, search: false },
    {
      title: '类型',
      dataIndex: 'Type',
      width: 90,
      render: (_v, r) => {
        const m = TYPE_META[r.Type]
        return m ? <Tag color={m.color}>{m.label}</Tag> : r.Type
      },
      renderFormItem: () => (
        <Select
          allowClear
          options={Object.entries(TYPE_META).map(([k, v]) => ({ value: k, label: v.label }))}
        />
      ),
    },
    {
      title: '面值',
      search: false,
      render: (_v, r) => <Text>{renderCouponValue(r)}</Text>,
    },
    {
      title: '有效期',
      search: false,
      width: 200,
      render: (_v, r) => (
        <Text type="secondary" style={{ fontSize: 12 }}>
          {fmtDate(r.ValidFrom)} ~ {fmtDate(r.ValidTo)}
        </Text>
      ),
    },
    {
      title: '状态',
      dataIndex: 'Status',
      width: 90,
      render: (_v, r) => {
        const m = STATUS_META[r.Status]
        return m ? <Tag color={m.color}>{m.label}</Tag> : r.Status
      },
      renderFormItem: () => (
        <Select
          allowClear
          options={Object.entries(STATUS_META).map(([k, v]) => ({ value: k, label: v.label }))}
        />
      ),
    },
    {
      title: '已发/总量',
      search: false,
      width: 100,
      render: (_v, r) => (
        <Text>
          {r.Issued}
          {r.TotalQuota > 0 ? ` / ${r.TotalQuota}` : ' / ∞'}
        </Text>
      ),
    },
    {
      title: '操作',
      search: false,
      width: 200,
      render: (_v, r) => (
        <Space size="small">
          {r.Status === 'draft' || r.Status === 'paused' ? (
            <Popconfirm
              title="确认上线该券?"
              onConfirm={() => handleUpdateStatus(r.ID, 'active')}
            >
              <Button type="link" size="small">上线</Button>
            </Popconfirm>
          ) : null}
          {r.Status === 'active' ? (
            <Popconfirm
              title="确认下线该券?"
              onConfirm={() => handleUpdateStatus(r.ID, 'paused')}
            >
              <Button type="link" size="small">下线</Button>
            </Popconfirm>
          ) : null}
          {r.Status !== 'ended' ? (
            <Popconfirm
              title="终止后不可恢复, 确认?"
              okText="终止"
              okButtonProps={{ danger: true }}
              onConfirm={() => handleUpdateStatus(r.ID, 'ended')}
            >
              <Button type="link" size="small" danger>终止</Button>
            </Popconfirm>
          ) : null}
          <Button
            type="link"
            size="small"
            onClick={() => {
              issueForm.resetFields()
              setIssueState({ open: true, coupon: r })
            }}
          >
            发券
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <div>
      <ProTable<Coupon>
        actionRef={actionRef}
        rowKey="ID"
        columns={columns}
        search={{ labelWidth: 80 }}
        request={async (params) => {
          try {
            const list = await listCoupons({
              status: params.Status as string | undefined,
              type: params.Type as string | undefined,
            })
            return {
              data: list ?? [],
              success: true,
              total: (list ?? []).length,
            }
          } catch {
            return { data: [], success: false, total: 0 }
          }
        }}
        pagination={{ pageSize: 10 }}
        toolBarRender={() => [
          <Button key="create" type="primary" onClick={() => setCreateOpen(true)}>
            新建券
          </Button>,
        ]}
        headerTitle="券模板列表"
        options={{ reload: true, density: false, fullScreen: false }}
      />

      {/* 新建券 Modal */}
      <Modal
        title="新建券"
        open={createOpen}
        onCancel={() => setCreateOpen(false)}
        onOk={handleCreate}
        confirmLoading={submitting}
        okText="创建"
        cancelText="取消"
        destroyOnClose
        width={520}
      >
        <Form form={createForm} layout="vertical" preserve={false}>
          <Form.Item name="name" label="券名称" rules={[{ required: true, message: '请输入券名称' }]}>
            <Input placeholder="如: 满100减20" maxLength={40} />
          </Form.Item>
          <Form.Item name="type" label="类型" rules={[{ required: true, message: '请选择类型' }]}>
            <Select
              placeholder="选择类型"
              options={Object.entries(TYPE_META).map(([k, v]) => ({ value: k, label: v.label }))}
            />
          </Form.Item>
          <Form.Item shouldUpdate={(p) => p.type} noStyle>
            {({ getFieldValue }) => {
              const t = getFieldValue('type') as string
              return (
                <Space direction="vertical" style={{ width: '100%' }}>
                  {(t === 'full_reduction' || t === 'cash') && (
                    <Form.Item name="off_cents" label="减免金额 (分)">
                      <InputNumber min={0} style={{ width: '100%' }} placeholder="如 2000 = ¥20" />
                    </Form.Item>
                  )}
                  {t === 'full_reduction' && (
                    <Form.Item name="threshold_cents" label="门槛金额 (分)">
                      <InputNumber min={0} style={{ width: '100%' }} placeholder="如 10000 = 满¥100" />
                    </Form.Item>
                  )}
                  {t === 'discount' && (
                    <Form.Item name="discount_permille" label="折扣 (‰, 850=85%=8.5折)">
                      <InputNumber min={0} max={1000} style={{ width: '100%' }} placeholder="如 850" />
                    </Form.Item>
                  )}
                  {t === 'gift' && (
                    <Form.Item name="gift_product_id" label="赠品商品 ID">
                      <InputNumber min={1} style={{ width: '100%' }} placeholder="商品 ID" />
                    </Form.Item>
                  )}
                </Space>
              )
            }}
          </Form.Item>
          <Space style={{ width: '100%' }}>
            <Form.Item name="valid_from" label="生效时间">
              <DatePicker showTime style={{ width: 220 }} />
            </Form.Item>
            <Form.Item
              name="valid_to"
              label="失效时间"
              rules={[{ required: true, message: '请选择失效时间' }]}
            >
              <DatePicker showTime style={{ width: 220 }} />
            </Form.Item>
          </Space>
          <Form.Item name="total_quota" label="发行总量 (0=不限)">
            <InputNumber min={0} style={{ width: '100%' }} placeholder="0 = 不限" />
          </Form.Item>
        </Form>
      </Modal>

      {/* 发券 Modal */}
      <Modal
        title={`发券 — ${issueState.coupon?.Name ?? ''}`}
        open={issueState.open}
        onCancel={() => setIssueState({ open: false })}
        onOk={handleIssue}
        confirmLoading={submitting}
        okText="发券"
        cancelText="取消"
        destroyOnClose
      >
        <Form form={issueForm} layout="vertical" preserve={false}>
          <Form.Item
            name="member_id"
            label="会员 ID"
            rules={[{ required: true, message: '请输入会员 ID' }]}
          >
            <InputNumber min={1} style={{ width: '100%' }} placeholder="目标会员 ID" />
          </Form.Item>
          <Form.Item label="券">
            <Space>
              <Text>{issueState.coupon?.Name}</Text>
              <Text type="secondary">{issueState.coupon ? renderCouponValue(issueState.coupon) : ''}</Text>
            </Space>
          </Form.Item>
          <Text type="secondary" style={{ fontSize: 12 }}>
            注: 发券可能触发毛利护栏 (errcode 60001), 由后端校验拦截, 失败时 toast 提示。
          </Text>
        </Form>
      </Modal>
    </div>
  )
}
