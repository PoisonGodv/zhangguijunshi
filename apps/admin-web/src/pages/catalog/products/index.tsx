// src/pages/catalog/products/index.tsx — 商品管理 (FE-ADMIN-02)
//
// ProTable 商品列表 (PascalCase 字段) + 新增商品 ModalForm + 改价 ModalForm +
// 按条码查搜索栏。后端 ListProducts 无真分页 total (count=本次条数), 这里拉大 limit
// 前端分页。
//
// 接口:
//   GET    /v1/boss/catalog/products              → getProducts
//   POST   /v1/boss/catalog/products              → createProduct
//   GET    /v1/boss/catalog/products/:id          → getProduct (行展开)
//   GET    /v1/boss/catalog/products/barcode/:barcode → getByBarcode (搜索)
//   PATCH  /v1/boss/catalog/products/:id/price   → updatePrice (改价)

import { useRef, useState } from 'react'
import { Button, Tag, Space, App } from 'antd'
import { PlusOutlined, ReloadOutlined, EditOutlined } from '@ant-design/icons'
import type { ActionType, ProColumns } from '@ant-design/pro-components'
import { ProTable, ModalForm, ProForm, ProFormText, ProFormDigit, ProFormSelect } from '@ant-design/pro-components'
import Money from '@/components/Money'
import { yuanText } from '@/utils/format'
import {
  getProducts,
  createProduct,
  getProduct,
  getByBarcode,
  updatePrice,
  type Product,
} from '@/services/catalog'

const STATUS_TAG: Record<string, { color: string; text: string }> = {
  active: { color: 'green', text: '在售' },
  archived: { color: 'default', text: '已下架' },
}

export default function ProductsPage() {
  const { message: msg } = App.useApp()
  const actionRef = useRef<ActionType>(null)
  const [createOpen, setCreateOpen] = useState(false)
  const [priceRow, setPriceRow] = useState<Product | null>(null)

  // 改价 ModalForm (受控, 按行打开)
  const [priceForm] = ProForm.useForm()
  const [priceOpen, setPriceOpen] = useState(false)

  const openPrice = async (row: Product) => {
    // 拉最新详情兜底 (避免列表字段滞后)
    try {
      const fresh = await getProduct(row.ID)
      setPriceRow(fresh)
      priceForm.setFieldsValue({
        cost_cents: fresh.CostCents ? fresh.CostCents / 100 : 0,
        price_cents: fresh.PriceCents ? fresh.PriceCents / 100 : 0,
      })
    } catch {
      setPriceRow(row)
      priceForm.setFieldsValue({
        cost_cents: row.CostCents ? row.CostCents / 100 : 0,
        price_cents: row.PriceCents ? row.PriceCents / 100 : 0,
      })
    }
    setPriceOpen(true)
  }

  const handleCreate = async (vals: Record<string, unknown>) => {
    await createProduct({
      name: String(vals.name ?? ''),
      barcode: vals.barcode ? String(vals.barcode) : undefined,
      category: vals.category ? String(vals.category) : undefined,
      unit: vals.unit ? String(vals.unit) : undefined,
      cost_cents: Math.round(Number(vals.cost_cents ?? 0) * 100),
      price_cents: Math.round(Number(vals.price_cents ?? 0) * 100),
      image_url: vals.image_url ? String(vals.image_url) : undefined,
      shelf_life_days: vals.shelf_life_days ? Number(vals.shelf_life_days) : undefined,
      tags: vals.tags ? String(vals.tags).split(',').map((s) => s.trim()).filter(Boolean) : undefined,
    })
    msg.success('商品已创建')
    setCreateOpen(false)
    actionRef.current?.reload()
    return true
  }

  const handlePrice = async (vals: Record<string, unknown>) => {
    if (!priceRow) return false
    const cost = Math.round(Number(vals.cost_cents ?? 0) * 100)
    const price = Math.round(Number(vals.price_cents ?? 0) * 100)
    if (price < cost) {
      msg.error('售价不能低于成本价')
      return false
    }
    if (price < 0 || cost < 0) {
      msg.error('金额不能为负')
      return false
    }
    await updatePrice(priceRow.ID, { cost_cents: cost, price_cents: price })
    msg.success('改价成功')
    setPriceOpen(false)
    actionRef.current?.reload()
    return true
  }

  const columns: ProColumns<Product>[] = [
    {
      title: 'ID',
      dataIndex: 'ID',
      width: 64,
      hideInSearch: true,
    },
    {
      title: '商品名称',
      dataIndex: 'Name',
      ellipsis: true,
      copyable: false,
    },
    {
      title: '条码',
      dataIndex: 'Barcode',
      width: 160,
      ellipsis: true,
    },
    {
      title: '分类',
      dataIndex: 'Category',
      width: 100,
      hideInSearch: true,
      render: (_, r) => r.Category || <Tag>未分类</Tag>,
    },
    {
      title: '单位',
      dataIndex: 'Unit',
      width: 70,
      hideInSearch: true,
    },
    {
      title: '成本价',
      dataIndex: 'CostCents',
      width: 110,
      hideInSearch: true,
      render: (_, r) => <Money value={r.CostCents} colorSign={false} />,
    },
    {
      title: '售价',
      dataIndex: 'PriceCents',
      width: 110,
      hideInSearch: true,
      render: (_, r) => <Money value={r.PriceCents} />,
    },
    {
      title: '状态',
      dataIndex: 'Status',
      width: 90,
      hideInSearch: true,
      valueType: 'select',
      valueEnum: {
        active: { text: '在售' },
        archived: { text: '已下架' },
      },
      render: (_, r) => {
        const t = STATUS_TAG[r.Status] || { color: 'default', text: r.Status }
        return <Tag color={t.color}>{t.text}</Tag>
      },
    },
    {
      title: '按条码查',
      dataIndex: 'barcode_search',
      hideInTable: true,
      fieldProps: { placeholder: '输入条码精确查询 (扫码)' },
    },
    {
      title: '关键词',
      dataIndex: 'query',
      hideInTable: true,
      fieldProps: { placeholder: '名称/条码模糊' },
    },
    {
      title: '操作',
      key: 'actions',
      width: 140,
      fixed: 'right',
      hideInSearch: true,
      render: (_, row) => (
        <Space size={0}>
          <Button type="link" size="small" icon={<EditOutlined />} onClick={() => openPrice(row)}>
            改价
          </Button>
        </Space>
      ),
    },
  ]

  return (
    <>
      <ProTable<Product>
        headerTitle="商品管理"
        actionRef={actionRef}
        rowKey="ID"
        columns={columns}
        scroll={{ x: 1100 }}
        search={{ labelWidth: 90, defaultCollapsed: false }}
        options={{ density: false, reload: true }}
        request={async (params) => {
          // 后端无真分页: 拉大 limit (最多 500), 前端分页。
          const limit = 500
          // 条码精确查优先 (命中即单条); 否则走 query 模糊。
          const barcode = (params.barcode_search as string | undefined)?.trim()
          if (barcode) {
            try {
              const p = await getByBarcode(barcode)
              return { data: [p], success: true, total: 1 }
            } catch {
              return { data: [], success: true, total: 0 }
            }
          }
          const query = (params.query as string | undefined)?.trim()
          const status = params.Status as string | undefined
          const resp = await getProducts({
            query: query || undefined,
            limit,
            offset: 0,
          })
          let items = resp.items || []
          // 状态前端二次过滤 (后端 ListProducts 不支持 status 维度)
          if (status) items = items.filter((p) => p.Status === status)
          const current = params.current ?? 1
          const pageSize = params.pageSize ?? 10
          const start = (current - 1) * pageSize
          const page = items.slice(start, start + pageSize)
          return { data: page, success: true, total: items.length }
        }}
        toolBarRender={() => [
          <Button
            key="create"
            type="primary"
            icon={<PlusOutlined />}
            onClick={() => setCreateOpen(true)}
          >
            新增商品
          </Button>,
          <Button
            key="reload"
            icon={<ReloadOutlined />}
            onClick={() => actionRef.current?.reload()}
          >
            刷新
          </Button>,
        ]}
      />

      <ModalForm
        title="新增商品"
        open={createOpen}
        onOpenChange={setCreateOpen}
        modalProps={{ destroyOnClose: true, maskClosable: false }}
        width={520}
        onFinish={handleCreate}
        submitter={{ searchConfig: { submitText: '创建', resetText: '取消' } }}
        grid
        rowProps={{ gutter: 16 }}
      >
        <ProFormText
          name="name"
          label="商品名称"
          colProps={{ span: 24 }}
          placeholder="例: 可口可乐 330ml"
          rules={[{ required: true, message: '请输入商品名称' }]}
        />
        <ProFormText
          name="barcode"
          label="条码"
          colProps={{ span: 12 }}
          placeholder="商品条码 (可选)"
        />
        <ProFormText
          name="unit"
          label="单位"
          colProps={{ span: 6 }}
          placeholder="瓶/个/斤"
        />
        <ProFormText
          name="category"
          label="分类"
          colProps={{ span: 6 }}
          placeholder="饮料/零食"
        />
        <ProFormDigit
          name="cost_cents"
          label="成本价 (元)"
          colProps={{ span: 12 }}
          min={0}
          precision={2}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0.00"
        />
        <ProFormDigit
          name="price_cents"
          label="售价 (元)"
          colProps={{ span: 12 }}
          min={0}
          precision={2}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0.00"
          rules={[{ required: true, message: '请输入售价' }]}
        />
        <ProFormDigit
          name="shelf_life_days"
          label="保质期(天)"
          colProps={{ span: 12 }}
          min={0}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0=未设"
        />
        <ProFormText
          name="tags"
          label="标签"
          colProps={{ span: 12 }}
          placeholder="逗号分隔, 例: 爆款,引流"
        />
        <ProFormText
          name="image_url"
          label="图片 URL"
          colProps={{ span: 24 }}
          placeholder="商品图片地址 (可选)"
        />
      </ModalForm>

      <ModalForm
        title={`改价 — ${priceRow?.Name ?? ''}`}
        open={priceOpen}
        onOpenChange={setPriceOpen}
        form={priceForm}
        modalProps={{ destroyOnClose: true, maskClosable: false }}
        width={440}
        onFinish={handlePrice}
        submitter={{ searchConfig: { submitText: '保存', resetText: '取消' } }}
        initialValues={{ cost_cents: 0, price_cents: 0 }}
      >
        <div style={{ marginBottom: 16 }}>
          <Tag color="gold">成本价 ≥ 0</Tag>
          <Tag color="orange">售价 ≥ 成本价</Tag>
        </div>
        <ProFormDigit
          name="cost_cents"
          label="成本价 (元)"
          min={0}
          precision={2}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0.00"
          rules={[{ required: true, message: '请输入成本价' }]}
        />
        <ProFormDigit
          name="price_cents"
          label="售价 (元)"
          min={0}
          precision={2}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0.00"
          rules={[{ required: true, message: '请输入售价' }]}
          extra={priceRow ? `当前售价 ${yuanText(priceRow.PriceCents)}` : undefined}
        />
        <ProFormSelect
          name="confirm"
          label="确认改价"
          colProps={{ span: 24 }}
          options={[
            { label: '我已确认价格正确', value: 1 },
          ]}
          rules={[{ required: true, message: '请确认' }]}
          placeholder="选择以确认"
        />
      </ModalForm>
    </>
  )
}
