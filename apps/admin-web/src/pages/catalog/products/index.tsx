// src/pages/catalog/products/index.tsx — 商品管理 (FE-ADMIN-02)
//
// ProTable 商品列表 (PascalCase 字段) + 新增商品 ModalForm + 改价 ModalForm +
// 按条码查搜索栏。CONTRACT §24.5 起 ListProducts 响应 {items,total} 真分页,
// 服务端分页 (limit/offset 随页码翻页), 不再拉大 limit 前端切片。
//
// 接口:
//   GET    /v1/boss/catalog/products              → getProducts
//   POST   /v1/boss/catalog/products              → createProduct
//   GET    /v1/boss/catalog/products/:id          → getProduct (行展开)
//   GET    /v1/boss/catalog/products/barcode/:barcode → getByBarcode (搜索)
//   PATCH  /v1/boss/catalog/products/:id/price   → updatePrice (改价)

import { useRef, useState } from 'react'
import { Button, Tag, Space, App, Modal, Upload, Input, Alert, List, Typography } from 'antd'
import { PlusOutlined, ReloadOutlined, EditOutlined, ImportOutlined, InboxOutlined } from '@ant-design/icons'
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
  importProducts,
  type Product,
  type CreateProductBody,
  type ImportResultResp,
} from '@/services/catalog'

const STATUS_TAG: Record<string, { color: string; text: string }> = {
  active: { color: 'green', text: '在售' },
  archived: { color: 'default', text: '已下架' },
}

// 单次导入上限 (对齐后端 importMaxRows=500)
const IMPORT_MAX_ROWS = 500

// 解析 CSV 文本为批量导入 items。列序: barcode,name,category,unit,cost_cents,
// price_cents[,image_url,shelf_life_days]; 金额列为「元」, 转分。首行若为表头
// (含 name/名称) 自动跳过; 简单 split(',') 解析 (不引重型库, 字段不含逗号)。
// 空行/列数不足的行被忽略并计入 skipped。
function parseCsvItems(text: string): { items: CreateProductBody[]; skipped: number } {
  const items: CreateProductBody[] = []
  let skipped = 0
  const lines = text.split(/\r?\n/)
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim()
    if (!line) continue
    const cols = line.split(',').map((c) => c.trim())
    // 表头行: 含 name/名称/barcode 之一即跳过
    if (i === 0 && /name|名称|barcode|条码/i.test(line)) continue
    if (cols.length < 6) {
      skipped++
      continue
    }
    const [barcode, name, category, unit, costStr, priceStr, imageUrl, shelfStr] = cols
    if (!name) {
      skipped++
      continue
    }
    const cost = Number(costStr)
    const price = Number(priceStr)
    if (Number.isNaN(cost) || Number.isNaN(price)) {
      skipped++
      continue
    }
    items.push({
      barcode: barcode || undefined,
      name,
      category: category || undefined,
      unit: unit || undefined,
      cost_cents: Math.round(cost * 100),
      price_cents: Math.round(price * 100),
      image_url: imageUrl || undefined,
      shelf_life_days: shelfStr ? Number(shelfStr) : undefined,
    })
  }
  return { items, skipped }
}

export default function ProductsPage() {
  const { message: msg } = App.useApp()
  const actionRef = useRef<ActionType>(null)
  const [createOpen, setCreateOpen] = useState(false)
  const [priceRow, setPriceRow] = useState<Product | null>(null)

  // 批量导入 (CONTRACT §25.6)
  const [importOpen, setImportOpen] = useState(false)
  const [csvText, setCsvText] = useState('')
  const [importing, setImporting] = useState(false)
  const [importResult, setImportResult] = useState<ImportResultResp | null>(null)
  const [parseSkipped, setParseSkipped] = useState(0)

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

  // 打开导入弹窗时重置状态
  const openImport = () => {
    setCsvText('')
    setImportResult(null)
    setParseSkipped(0)
    setImportOpen(true)
  }

  // 读取上传的 CSV 文件内容到 textarea (统一走文本解析链路)
  const readCsvFile = (file: File) => {
    const reader = new FileReader()
    reader.onload = () => {
      setCsvText(String(reader.result ?? ''))
      setImportResult(null)
    }
    reader.readAsText(file)
    return false // 阻止 antd 默认上传行为
  }

  // 提交批量导入: 解析 → 调 import 接口 → 展示成功/失败明细
  const handleImport = async () => {
    const { items, skipped } = parseCsvItems(csvText)
    setParseSkipped(skipped)
    if (items.length === 0) {
      msg.warning('未解析到有效商品行, 请检查 CSV 内容')
      return
    }
    if (items.length > IMPORT_MAX_ROWS) {
      msg.error(`单次最多导入 ${IMPORT_MAX_ROWS} 条, 当前解析出 ${items.length} 条`)
      return
    }
    setImporting(true)
    try {
      const res = await importProducts(items)
      setImportResult(res)
      if (res.failed === 0) {
        msg.success(`成功导入 ${res.imported} 条`)
      } else {
        msg.warning(`导入完成: 成功 ${res.imported} 条, 失败 ${res.failed} 条`)
      }
      actionRef.current?.reload()
    } catch {
      // request.ts 拦截器已弹出后端错误提示
    } finally {
      setImporting(false)
    }
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
          // CONTRACT §24.5: 响应 {items,total} 真分页 → 服务端分页。
          const current = params.current ?? 1
          const pageSize = params.pageSize ?? 10
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
            limit: pageSize,
            offset: (current - 1) * pageSize,
          })
          let items = resp.items || []
          // 状态前端二次过滤 (后端 ListProducts 不支持 status 维度; total 以未过滤口径为准)
          if (status) items = items.filter((p) => p.Status === status)
          return { data: items, success: true, total: resp.total ?? items.length }
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
            key="import"
            icon={<ImportOutlined />}
            onClick={openImport}
          >
            批量导入
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

      {/* 批量导入 (CONTRACT §25.6): Upload.Dragger 选 CSV 或粘贴 CSV 文本 → 解析 → import 接口 → 明细 */}
      <Modal
        title="批量导入商品"
        open={importOpen}
        onCancel={() => setImportOpen(false)}
        width={640}
        confirmLoading={importing}
        okText="开始导入"
        cancelText="关闭"
        onOk={handleImport}
        destroyOnClose
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message="CSV 列序: barcode,name,category,unit,cost_cents,price_cents(,image_url,shelf_life_days 可选)"
          description="金额为「元」(自动转分); 首行表头可跳过; 单次最多 500 条; 单条失败不中断, 返回每行错误。"
        />
        <Upload.Dragger
          accept=".csv,.txt"
          maxCount={1}
          beforeUpload={(file) => readCsvFile(file)}
          onRemove={() => setCsvText('')}
          style={{ marginBottom: 12 }}
        >
          <p className="ant-upload-drag-icon">
            <InboxOutlined />
          </p>
          <p className="ant-upload-text">点击或拖拽 CSV 文件到此处</p>
          <p className="ant-upload-hint">也可直接在下方文本框粘贴 CSV 内容</p>
        </Upload.Dragger>
        <Input.TextArea
          rows={6}
          value={csvText}
          onChange={(e) => {
            setCsvText(e.target.value)
            setImportResult(null)
          }}
          placeholder={'barcode,name,category,unit,cost_cents,price_cents\n6900001,可口可乐,饮料,瓶,1.50,3.00'}
        />
        {parseSkipped > 0 && !importResult && (
          <Typography.Text type="warning" style={{ display: 'block', marginTop: 8 }}>
            上次解析有 {parseSkipped} 行无效被跳过
          </Typography.Text>
        )}
        {importResult && (
          <div style={{ marginTop: 16 }}>
            <Alert
              type={importResult.failed === 0 ? 'success' : 'warning'}
              showIcon
              message={`导入完成: 成功 ${importResult.imported} 条, 失败 ${importResult.failed} 条`}
            />
            {importResult.errors && importResult.errors.length > 0 && (
              <List
                size="small"
                style={{ marginTop: 8, maxHeight: 200, overflow: 'auto' }}
                header={<Typography.Text strong>失败明细</Typography.Text>}
                bordered
                dataSource={importResult.errors}
                renderItem={(e) => (
                  <List.Item>
                    <Typography.Text type="danger">
                      第 {e.row} 行: {e.reason}
                    </Typography.Text>
                  </List.Item>
                )}
              />
            )}
          </div>
        )}
      </Modal>
    </>
  )
}
