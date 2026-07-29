// src/pages/inventory/stock/index.tsx — 库存管理 (FE-ADMIN-02)
//
// Tab 切换 "批次库存" | "出入库流水"。批次库存页: 按商品 ID 查当前总库存 (GET /stock)
// + 近期入库流水 (purchase_in) 侧面呈现批次; 流水页: 全量 Movement。
// 工具栏 "入库" ModalForm 调 purchase; 行操作 "消耗扣减" 调 consume (FIFO)。
//
// 后端约束 (核实后):
//   - 无 list-batches 接口; GET /stock 仅返某商品当前总余量 (quantity 小写)。
//   - GET /movements 返全量 []Movement (无 total, 无分页) → 前端分页。
//   - Movement.BatchID + Quantity(正) 可侧面还原入库批次; Batch.ExpiryDate 无法
//     在列表展示 (无批次列表接口) → 临期标记仅在流水行的 Expiry 无从获取时,
//     用 Movement.Type + CreatedAt 近似; 真正临期看板需后端补 batches 接口 (见 TODO)。

import { useEffect, useRef, useState } from 'react'
import { Button, Tag, Tabs, App } from 'antd'
import { PlusOutlined, ReloadOutlined, MinusOutlined } from '@ant-design/icons'
import type { ActionType, ProColumns } from '@ant-design/pro-components'
import { ProTable, ModalForm, ProForm, ProFormDigit, ProFormText, ProFormTextArea } from '@ant-design/pro-components'
import { fmtDateTime, fmtDate } from '@/utils/format'
import {
  purchase,
  listMovements,
  consume,
  type Movement,
} from '@/services/inventory'
import { getProducts } from '@/services/catalog'

// 流水类型 → Tag 颜色
const MOVE_TYPE_TAG: Record<string, { color: string; text: string }> = {
  purchase_in: { color: 'green', text: '入库' },
  sale_out: { color: 'red', text: '销售出库' },
  stocktake_adj: { color: 'gold', text: '盘点调整' },
  return_in: { color: 'blue', text: '退货入库' },
  loss: { color: 'volcano', text: '报损' },
  transfer: { color: 'purple', text: '调拨' },
}

// 临期判定: expire 字段近 30 天 → 红色 Tag (用于批次列表; 流水无 expire 字段时跳过)
function expiryTag(expiry: string | null | undefined): React.ReactNode {
  if (!expiry) return <Tag>无保质期</Tag>
  const d = new Date(expiry)
  if (isNaN(d.getTime())) return <Tag>{expiry}</Tag>
  const days = Math.ceil((d.getTime() - Date.now()) / 86400000)
  if (days < 0) return <Tag color="red">已过期</Tag>
  if (days <= 30) return <Tag color="red">临期 {days}天</Tag>
  return <Tag color="green">{fmtDate(expiry)}</Tag>
}

export default function StockPage() {
  const { message: msg } = App.useApp()
  const stockRef = useRef<ActionType>(null)
  const moveRef = useRef<ActionType>(null)
  const [tab, setTab] = useState<'batches' | 'movements'>('batches')
  const [purchaseOpen, setPurchaseOpen] = useState(false)
  const [consumeRow, setConsumeRow] = useState<Movement | null>(null)
  const [consumeOpen, setConsumeOpen] = useState(false)
  const [consumeForm] = ProForm.useForm()

  // === 入库 ===
  const handlePurchase = async (vals: Record<string, unknown>) => {
    const productId = Number(vals.product_id ?? 0)
    if (!productId || productId <= 0) {
      msg.error('请输入有效商品 ID')
      return false
    }
    const qty = Number(vals.quantity ?? 0)
    if (!qty || qty <= 0) {
      msg.error('入库数量必须 > 0')
      return false
    }
    await purchase({
      product_id: productId,
      quantity: qty,
      in_price_cents: vals.in_price_cents ? Math.round(Number(vals.in_price_cents) * 100) : undefined,
      expiry_date: vals.expiry_date ? String(vals.expiry_date) : undefined,
      note: vals.note ? String(vals.note) : undefined,
    })
    msg.success('入库成功')
    setPurchaseOpen(false)
    stockRef.current?.reload()
    moveRef.current?.reload()
    return true
  }

  // === 消耗扣库存 (FIFO) ===
  const handleConsume = async (vals: Record<string, unknown>) => {
    if (!consumeRow) return false
    const qty = Number(vals.qty ?? 0)
    if (!qty || qty <= 0) {
      msg.error('消耗数量必须 > 0')
      return false
    }
    await consume(consumeRow.ProductID, {
      qty,
      ref_order_id: vals.ref_order_id ? Number(vals.ref_order_id) : undefined,
    })
    msg.success('消耗扣减成功')
    setConsumeOpen(false)
    stockRef.current?.reload()
    moveRef.current?.reload()
    return true
  }

  // === 批次库存列 (按 Movement purchase_in 行呈现, 侧面还原批次) ===
  const batchColumns: ProColumns<Movement>[] = [
    { title: '流水 ID', dataIndex: 'ID', width: 80, hideInSearch: true },
    {
      title: '商品 ID',
      dataIndex: 'ProductID',
      width: 90,
      hideInSearch: true,
    },
    {
      title: '商品名称',
      dataIndex: 'ProductName',
      width: 140,
      hideInSearch: true,
      render: (_, r) => <ProductNameCell productID={r.ProductID} />,
    },
    {
      title: '入库数量',
      dataIndex: 'Quantity',
      width: 100,
      hideInSearch: true,
      render: (_, r) => <Tag color="green">+{r.Quantity}</Tag>,
    },
    {
      title: '进货价',
      dataIndex: 'InPriceCents',
      width: 110,
      hideInSearch: true,
      render: () => {
        // Movement 无 InPriceCents; purchase 返的 Batch 才有. 流水行只能从 note 侧面看.
        // 这里用 product 当前售价近似展示 (TODO: 后端补批次列表后改 Batch.InPriceCents)
        return <span style={{ color: '#999' }}>—</span>
      },
    },
    {
      title: '保质期',
      dataIndex: 'ExpiryDate',
      width: 130,
      hideInSearch: true,
      render: (_, r) => {
        // Movement 无 ExpiryDate; 近期可从 note 解析 YYYY-MM-DD (入库时填了)
        const m = r.Note?.match(/\d{4}-\d{2}-\d{2}/)
        return expiryTag(m ? m[0] : null)
      },
    },
    {
      title: '入库时间',
      dataIndex: 'CreatedAt',
      width: 160,
      hideInSearch: true,
      render: (_, r) => fmtDateTime(r.CreatedAt),
    },
    {
      title: '备注',
      dataIndex: 'Note',
      ellipsis: true,
      hideInSearch: true,
    },
    {
      title: '商品 ID 查',
      dataIndex: 'product_id_search',
      hideInTable: true,
      render: () => null,
    },
    {
      title: '操作',
      key: 'actions',
      width: 120,
      fixed: 'right',
      hideInSearch: true,
      render: (_, row) => (
        <Button
          type="link"
          size="small"
          icon={<MinusOutlined />}
          onClick={() => {
            setConsumeRow(row)
            consumeForm.setFieldsValue({ qty: row.Quantity, ref_order_id: undefined })
            setConsumeOpen(true)
          }}
        >
          消耗扣减
        </Button>
      ),
    },
  ]

  // === 流水列 ===
  const moveColumns: ProColumns<Movement>[] = [
    { title: '流水 ID', dataIndex: 'ID', width: 80, hideInSearch: true },
    {
      title: '商品 ID',
      dataIndex: 'ProductID',
      width: 90,
      hideInSearch: true,
    },
    {
      title: '商品名称',
      dataIndex: 'ProductName',
      width: 140,
      hideInSearch: true,
      render: (_, r) => <ProductNameCell productID={r.ProductID} />,
    },
    {
      title: '批次 ID',
      dataIndex: 'BatchID',
      width: 90,
      hideInSearch: true,
      render: (_, r) => (r.BatchID ? r.BatchID : <Tag>—</Tag>),
    },
    {
      title: '类型',
      dataIndex: 'Type',
      width: 110,
      valueType: 'select',
      valueEnum: {
        purchase_in: { text: '入库' },
        sale_out: { text: '销售出库' },
        stocktake_adj: { text: '盘点调整' },
        return_in: { text: '退货入库' },
        loss: { text: '报损' },
        transfer: { text: '调拨' },
      },
      render: (_, r) => {
        const t = MOVE_TYPE_TAG[r.Type] || { color: 'default', text: r.Type }
        return <Tag color={t.color}>{t.text}</Tag>
      },
    },
    {
      title: '数量',
      dataIndex: 'Quantity',
      width: 90,
      hideInSearch: true,
      render: (_, r) => {
        const isPos = r.Quantity >= 0
        return <Tag color={isPos ? 'green' : 'red'}>{isPos ? '+' : ''}{r.Quantity}</Tag>
      },
    },
    {
      title: '关联订单',
      dataIndex: 'RefOrderID',
      width: 100,
      hideInSearch: true,
      render: (_, r) => (r.RefOrderID ? r.RefOrderID : <Tag>—</Tag>),
    },
    {
      title: '操作人',
      dataIndex: 'OperatorID',
      width: 80,
      hideInSearch: true,
      render: (_, r) => r.OperatorID || <Tag>—</Tag>,
    },
    { title: '备注', dataIndex: 'Note', ellipsis: true, hideInSearch: true },
    {
      title: '时间',
      dataIndex: 'CreatedAt',
      width: 160,
      hideInSearch: true,
      render: (_, r) => fmtDateTime(r.CreatedAt),
    },
    {
      title: '商品 ID 查',
      dataIndex: 'product_id_search',
      hideInTable: true,
      render: () => null,
    },
    {
      title: '日期',
      dataIndex: 'date',
      valueType: 'date',
      hideInTable: true,
      render: () => null,
    },
    {
      title: '类型',
      dataIndex: 'type_search',
      hideInTable: true,
      valueType: 'select',
      valueEnum: {
        purchase_in: { text: '入库' },
        sale_out: { text: '销售出库' },
        stocktake_adj: { text: '盘点调整' },
        return_in: { text: '退货入库' },
        loss: { text: '报损' },
        transfer: { text: '调拨' },
      },
      render: () => null,
    },
  ]

  return (
    <>
      <Tabs
        activeKey={tab}
        onChange={(k) => setTab(k as 'batches' | 'movements')}
        items={[
          {
            key: 'batches',
            label: '批次库存',
            children: (
              <ProTable<Movement>
                headerTitle="批次库存 (按入库流水呈现)"
                actionRef={stockRef}
                rowKey="ID"
                columns={batchColumns}
                scroll={{ x: 1200 }}
                search={{ labelWidth: 100, defaultCollapsed: false }}
                options={{ density: false, reload: true }}
                request={async (params) => {
                  // 后端无批次列表接口; 用 purchase_in 流水侧面还原批次.
                  const productId = (params.product_id_search as number | undefined) ?? undefined
                  const resp = await listMovements({
                    product_id: productId,
                    type: 'purchase_in',
                    limit: 500,
                    offset: 0,
                  })
                  let items = resp || []
                  // 倒序: 最新入库在前
                  items = [...items].sort((a, b) => +new Date(b.CreatedAt) - +new Date(a.CreatedAt))
                  const current = params.current ?? 1
                  const pageSize = params.pageSize ?? 10
                  const start = (current - 1) * pageSize
                  const page = items.slice(start, start + pageSize)
                  return { data: page, success: true, total: items.length }
                }}
                toolBarRender={() => [
                  <Button
                    key="purchase"
                    type="primary"
                    icon={<PlusOutlined />}
                    onClick={() => setPurchaseOpen(true)}
                  >
                    入库
                  </Button>,
                  <Button
                    key="reload"
                    icon={<ReloadOutlined />}
                    onClick={() => stockRef.current?.reload()}
                  >
                    刷新
                  </Button>,
                ]}
              />
            ),
          },
          {
            key: 'movements',
            label: '出入库流水',
            children: (
              <ProTable<Movement>
                headerTitle="出入库流水"
                actionRef={moveRef}
                rowKey="ID"
                columns={moveColumns}
                scroll={{ x: 1200 }}
                search={{ labelWidth: 100, defaultCollapsed: false }}
                options={{ density: false, reload: true }}
                request={async (params) => {
                  const productId = (params.product_id_search as number | undefined) ?? undefined
                  const type = (params.type_search as string | undefined) ?? undefined
                  const date = params.date as string | undefined
                  const dateStr = date ? fmtDate(date) : undefined
                  const resp = await listMovements({
                    product_id: productId,
                    type: type,
                    date: dateStr,
                    limit: 500,
                    offset: 0,
                  })
                  let items = resp || []
                  items = [...items].sort((a, b) => +new Date(b.CreatedAt) - +new Date(a.CreatedAt))
                  const current = params.current ?? 1
                  const pageSize = params.pageSize ?? 10
                  const start = (current - 1) * pageSize
                  const page = items.slice(start, start + pageSize)
                  return { data: page, success: true, total: items.length }
                }}
                toolBarRender={() => [
                  <Button
                    key="reload"
                    icon={<ReloadOutlined />}
                    onClick={() => moveRef.current?.reload()}
                  >
                    刷新
                  </Button>,
                ]}
              />
            ),
          },
        ]}
      />

      <ModalForm
        title="入库"
        open={purchaseOpen}
        onOpenChange={setPurchaseOpen}
        modalProps={{ destroyOnClose: true, maskClosable: false }}
        width={520}
        onFinish={handlePurchase}
        submitter={{ searchConfig: { submitText: '入库', resetText: '取消' } }}
        grid
        rowProps={{ gutter: 16 }}
      >
        <ProFormDigit
          name="product_id"
          label="商品 ID"
          colProps={{ span: 12 }}
          min={1}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="商品 ID"
          rules={[{ required: true, message: '请输入商品 ID' }]}
        />
        <ProFormDigit
          name="quantity"
          label="入库数量"
          colProps={{ span: 12 }}
          min={1}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="数量"
          rules={[{ required: true, message: '请输入数量' }]}
        />
        <ProFormDigit
          name="in_price_cents"
          label="进货价 (元)"
          colProps={{ span: 12 }}
          min={0}
          precision={2}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="0.00"
        />
        <ProFormText
          name="expiry_date"
          label="保质期至"
          colProps={{ span: 12 }}
          placeholder="YYYY-MM-DD (可选)"
        />
        <ProFormTextArea
          name="note"
          label="备注"
          colProps={{ span: 24 }}
          placeholder="批次号/供应商等 (可选)"
          fieldProps={{ rows: 2 }}
        />
      </ModalForm>

      <ModalForm
        title={`消耗扣减 — 商品 ${consumeRow?.ProductID ?? ''}`}
        open={consumeOpen}
        onOpenChange={setConsumeOpen}
        form={consumeForm}
        modalProps={{ destroyOnClose: true, maskClosable: false }}
        width={440}
        onFinish={handleConsume}
        submitter={{ searchConfig: { submitText: '扣减', resetText: '取消' } }}
        initialValues={{ qty: 1 }}
      >
        <div style={{ marginBottom: 16 }}>
          <Tag color="orange">FIFO 先进先出</Tag>
          <Tag color="red">库存不足会整体失败 (不部分扣)</Tag>
        </div>
        <ProFormDigit
          name="qty"
          label="消耗数量"
          min={1}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="数量"
          rules={[{ required: true, message: '请输入数量' }]}
        />
        <ProFormDigit
          name="ref_order_id"
          label="关联订单 ID"
          min={0}
          fieldProps={{ style: { width: '100%' } }}
          placeholder="可选"
        />
      </ModalForm>
    </>
  )
}

// === 商品名称缓存展示 (全表共享一次拉取, 避免每行请求) ===
const productNameCache = new Map<number, string>()
let productNamePromise: Promise<Map<number, string>> | null = null
function loadProductNames(): Promise<Map<number, string>> {
  if (productNamePromise) return productNamePromise
  productNamePromise = getProducts({ limit: 500 })
    .then((resp) => {
      for (const p of resp.items || []) {
        productNameCache.set(p.ID, p.Name)
      }
      return productNameCache
    })
    .catch(() => productNameCache)
  return productNamePromise
}

function ProductNameCell({ productID }: { productID: number }) {
  const [name, setName] = useState<string>(productNameCache.get(productID) || `#${productID}`)
  useEffect(() => {
    if (productNameCache.has(productID)) {
      setName(productNameCache.get(productID) || `#${productID}`)
      return
    }
    let alive = true
    loadProductNames().then((m) => {
      if (alive) setName(m.get(productID) || `#${productID}`)
    })
    return () => {
      alive = false
    }
  }, [productID])
  return <span>{name}</span>
}
