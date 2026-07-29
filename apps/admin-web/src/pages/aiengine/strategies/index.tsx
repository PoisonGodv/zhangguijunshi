// src/pages/aiengine/strategies/index.tsx — AI 策略 (FE-ADMIN-02)
//
// 布局: 左侧"今日3件事" (POST /v1/boss/aiengine/today → Advice.strategy.items),
//        下方 listStrategies() ProTable (id/标题/状态/创建时间)。
// 点击策略 → Drawer: getStrategy(id) 展示 StrategyItem 列表, 每条"一键执行"调
//   executeItem → 返 {status: executed|blocked|failed, reason?};
//   blocked 时 Alert 展示归因 reason。Drawer 内"归因看板"区调 getAttribution(id),
//   ECharts 展示各 item 的 coupon_redeem/visit_uplift/groupbuy_success/repurchase/
//   revenue_cny (缺失为 null, 看板显示"—")。
//
// aiengine 全栈 snake_case; 200-as-data 模式: blocked 不是 errcode, 按 status 字段分支。

import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  Card,
  Col,
  Row,
  Space,
  Tag,
  Typography,
  Button,
  Drawer,
  List,
  Alert,
  Spin,
  Tooltip,
} from 'antd'
import { ProTable, type ProColumns } from '@ant-design/pro-components'
import ReactECharts from 'echarts-for-react'
import {
  today,
  listStrategies,
  getStrategy,
  executeItem,
  getAttribution,
  type Advice,
  type Strategy,
  type StrategyItem,
  type ExecResult,
  type Attribution,
} from '@/services/aiengine'
import { fmtDateTime } from '@/utils/format'
import EmptyState from '@/components/EmptyState'
import PageLoading from '@/components/PageLoading'

const { Text, Paragraph } = Typography

// 动作类型 → 中文
const ACTION_LABEL: Record<string, string> = {
  group_seckill: '社群秒杀',
  coupon_wake: '券唤醒',
  moments_post: '朋友圈',
  price_adj: '调价',
  groupbuy: '拼团',
  recharge_promo: '储值促',
}

// 执行状态 → Tag
const EXEC_STATUS_TAG: Record<string, { label: string; color: string }> = {
  pending: { label: '待执行', color: 'default' },
  executed: { label: '已执行', color: 'green' },
  skipped: { label: '已跳过', color: 'gold' },
  failed: { label: '失败', color: 'red' },
  blocked: { label: '已拦截', color: 'orange' },
}

// 策略状态 → Tag
const STATUS_TAG: Record<string, { label: string; color: string }> = {
  generated: { label: '已生成', color: 'blue' },
  partially_executed: { label: '部分执行', color: 'gold' },
  executed: { label: '已执行', color: 'green' },
  dismissed: { label: '已忽略', color: 'default' },
}

// 降级原因 → 中文
const DEGRADE_LABEL: Record<string, string> = {
  budget: 'AI 预算熔断',
  llm: 'LLM 调用失败',
  no_provider: '无可用 LLM',
  cold_start: '冷启动',
  cache_hit: '缓存命中',
}

// 执行 blocked reason → 中文 (exec.ExecResult.Reason)
const REASON_LABEL: Record<string, string> = {
  margin_floor: '毛利护栏拦截',
  freq_circuit: '频次熔断',
  content_filter: '内容敏感词拦截',
  unsubscribed: '会员已退订',
  subscribe_quota: '订阅次数用尽',
  confirm_required: '需二次确认',
  not_executable: '动作暂不可执行',
  not_configured: '依赖未配置',
  send_failed: '触达发送失败',
  invalid_item: '策略项参数缺失',
  blocked: '已拦截',
}

function fmtReason(reason?: string): string {
  if (!reason) return ''
  return REASON_LABEL[reason] ?? reason
}

export default function StrategiesPage() {
  const [advice, setAdvice] = useState<Advice | null>(null)
  const [adviceLoading, setAdviceLoading] = useState(false)
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [detail, setDetail] = useState<Strategy | null>(null)
  const [detailLoading, setDetailLoading] = useState(false)
  const [attribution, setAttribution] = useState<Attribution | null>(null)
  const [attrLoading, setAttrLoading] = useState(false)
  const [execResults, setExecResults] = useState<Record<number, ExecResult>>({})
  const [executing, setExecuting] = useState<Set<number>>(new Set())

  // 拉取今日3件事
  const loadAdvice = useCallback(async () => {
    setAdviceLoading(true)
    try {
      const a = await today()
      setAdvice(a)
    } catch {
      setAdvice(null)
    } finally {
      setAdviceLoading(false)
    }
  }, [])

  useEffect(() => {
    void loadAdvice()
  }, [loadAdvice])

  // 打开策略详情 Drawer: 加载 strategy + attribution
  const openDetail = useCallback(async (id: number) => {
    setDrawerOpen(true)
    setDetail(null)
    setAttribution(null)
    setExecResults({})
    setDetailLoading(true)
    try {
      const [s, attr] = await Promise.all([
        getStrategy(id),
        getAttribution(id).catch(() => null),
      ])
      setDetail(s)
      setAttribution(attr)
    } catch {
      // request.ts 已 toast
    } finally {
      setDetailLoading(false)
    }
  }, [])

  // 归因加载 (Drawer 内手动刷新)
  const reloadAttribution = useCallback(async (id: number) => {
    setAttrLoading(true)
    try {
      const attr = await getAttribution(id).catch(() => null)
      setAttribution(attr)
    } finally {
      setAttrLoading(false)
    }
  }, [])

  // 一键执行 (200-as-data: blocked 也 200, 按 status 分支)
  const handleExecute = useCallback(
    async (item: StrategyItem) => {
      // price_adj 需要二次确认, 用 window.confirm 简化
      let confirm = false
      if (item.action_type === 'price_adj') {
        confirm = window.confirm('调价为高风险动作, 确认执行?')
        if (!confirm) return
      }
      setExecuting((prev) => new Set(prev).add(item.id))
      try {
        const res = await executeItem(item.strategy_id, item.id, item.action_type === 'price_adj' ? true : undefined)
        setExecResults((prev) => ({ ...prev, [item.id]: res }))
        // 执行后刷新详情 (item.execution_status 会变)
        const s = await getStrategy(item.strategy_id).catch(() => null)
        if (s) setDetail(s)
      } catch {
        // request.ts 已 toast (真实系统错才 reject)
      } finally {
        setExecuting((prev) => {
          const next = new Set(prev)
          next.delete(item.id)
          return next
        })
      }
    },
    [],
  )

  // 策略列表列
  const columns: ProColumns<Strategy>[] = [
    { title: 'ID', dataIndex: 'id', width: 70 },
    {
      title: '日期',
      dataIndex: 'for_date',
      width: 110,
      render: (v) => <Text>{v as string}</Text>,
    },
    {
      title: '生成方式',
      dataIndex: 'generated_by',
      width: 100,
      render: (v) => <Tag>{(v as string) || '-'}</Tag>,
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 100,
      render: (v) => {
        const m = STATUS_TAG[v as string]
        return m ? <Tag color={m.color}>{m.label}</Tag> : (v as string)
      },
    },
    {
      title: '事项数',
      dataIndex: 'items',
      width: 80,
      render: (_v, r) => <Text>{r.items?.length ?? 0}</Text>,
      search: false,
    },
    {
      title: '创建时间',
      dataIndex: 'created_at',
      width: 160,
      render: (v) => <Text type="secondary">{fmtDateTime(v as string)}</Text>,
      search: false,
    },
    {
      title: '操作',
      search: false,
      width: 100,
      render: (_v, r) => (
        <Button type="link" size="small" onClick={() => openDetail(r.id)}>
          查看详情
        </Button>
      ),
    },
  ]

  // 归因 ECharts: 各 item 的多维指标条形图 (coupon_redeem / groupbuy_success / repurchase 为 0-1 比率)
  const attrOption = useMemo(() => {
    const items = attribution?.items ?? []
    if (items.length === 0) return null
    const labels = items.map((it) => `#${it.rank} ${ACTION_LABEL[it.action_type] ?? it.action_type}`)
    const pct = (v?: number | null): number | null => (v == null ? null : v)
    return {
      tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
      legend: { data: ['核销率', '成团率', '复购率', '到店增量'] },
      grid: { left: '12%', right: '8%', top: 40, bottom: 30 },
      xAxis: { type: 'category', data: labels, axisLabel: { interval: 0, rotate: 0 } },
      yAxis: { type: 'value', max: 1, axisLabel: { formatter: (v: number) => (v * 100).toFixed(0) + '%' } },
      series: [
        {
          name: '核销率',
          type: 'bar',
          data: items.map((it) => pct(it.coupon_redeem)),
          itemStyle: { color: '#FF6B35' },
        },
        {
          name: '成团率',
          type: 'bar',
          data: items.map((it) => pct(it.groupbuy_success)),
          itemStyle: { color: '#52c41a' },
        },
        {
          name: '复购率',
          type: 'bar',
          data: items.map((it) => pct(it.repurchase)),
          itemStyle: { color: '#1677ff' },
        },
        {
          name: '到店增量',
          type: 'bar',
          data: items.map((it) => pct(it.visit_uplift)),
          itemStyle: { color: '#722ed1' },
        },
      ],
    }
  }, [attribution])

  return (
    <div>
      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={10}>
          <Card
            title="今日3件事"
            extra={
              <Space>
                {advice?.degrade ? (
                  <Tooltip title={`degrade=${advice.degrade}`}>
                    <Tag color="orange">{DEGRADE_LABEL[advice.degrade] ?? advice.degrade}</Tag>
                  </Tooltip>
                ) : null}
                <Button size="small" onClick={() => loadAdvice()} loading={adviceLoading}>
                  刷新
                </Button>
              </Space>
            }
            styles={{ body: { padding: 12 } }}
          >
            {adviceLoading && !advice ? (
              <div style={{ textAlign: 'center', padding: 24 }}>
                <Spin />
              </div>
            ) : advice?.strategy?.items?.length ? (
              <List
                dataSource={advice.strategy.items}
                renderItem={(item) => (
                  <List.Item>
                    <Space align="start" style={{ width: '100%' }}>
                      <Tag color="orange" style={{ marginTop: 2 }}>
                        #{item.rank}
                      </Tag>
                      <div style={{ flex: 1 }}>
                        <Space size="small" wrap>
                          <Text strong>{ACTION_LABEL[item.action_type] ?? item.action_type}</Text>
                          <Tag>{item.margin_ok ? '毛利OK' : '毛利风险'}</Tag>
                          <Tag>{EXEC_STATUS_TAG[item.execution_status]?.label ?? item.execution_status}</Tag>
                        </Space>
                        <Paragraph style={{ margin: '4px 0 0', fontSize: 13 }}>
                          {item.copy}
                        </Paragraph>
                        {item.reason ? (
                          <Text type="secondary" style={{ fontSize: 12 }}>
                            归因: {item.reason}
                          </Text>
                        ) : null}
                      </div>
                    </Space>
                  </List.Item>
                )}
              />
            ) : (
              <EmptyState title="暂无今日建议" description="点击刷新重新生成, 或检查数据质量分" />
            )}
          </Card>
        </Col>
        <Col span={14}>
          <Card title="策略回放" styles={{ body: { padding: 0 } }}>
            <ProTable<Strategy>
              rowKey="id"
              columns={columns}
              search={false}
              request={async () => {
                try {
                  const list = await listStrategies()
                  return { data: list ?? [], success: true, total: (list ?? []).length }
                } catch {
                  return { data: [], success: false, total: 0 }
                }
              }}
              pagination={{ pageSize: 5 }}
              options={{ reload: true, density: false, fullScreen: false }}
              headerTitle="历史策略"
            />
          </Card>
        </Col>
      </Row>

      <Drawer
        width={760}
        title={detail ? `策略 #${detail.id} — ${detail.for_date}` : '策略详情'}
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        destroyOnClose
      >
        {detailLoading ? (
          <PageLoading tip="加载策略详情..." />
        ) : detail ? (
          <div>
            <Space direction="vertical" style={{ width: '100%' }} size="middle">
              <Card size="small">
                <Space size="large" wrap>
                  <Text>
                    生成方式: <Tag>{detail.generated_by}</Tag>
                  </Text>
                  <Text>
                    状态:{' '}
                    {(() => {
                      const m = STATUS_TAG[detail.status]
                      return m ? <Tag color={m.color}>{m.label}</Tag> : detail.status
                    })()}
                  </Text>
                  <Text>数据质量分: {detail.data_quality_score}</Text>
                  {detail.llm_tier ? <Text>LLM: {detail.llm_tier}</Text> : null}
                </Space>
              </Card>

              <Card title="策略事项 (一键执行)" size="small">
                <List
                  dataSource={detail.items ?? []}
                  renderItem={(item) => {
                    const res = execResults[item.id]
                    const isExecuting = executing.has(item.id)
                    return (
                      <List.Item>
                        <Space align="start" style={{ width: '100%' }} direction="vertical">
                          <Space size="small" wrap>
                            <Tag color="orange">#{item.rank}</Tag>
                            <Text strong>{ACTION_LABEL[item.action_type] ?? item.action_type}</Text>
                            <Tag>{item.reason_type}</Tag>
                            {item.margin_ok ? (
                              <Tag color="green">毛利OK</Tag>
                            ) : (
                              <Tag color="red">毛利风险</Tag>
                            )}
                            <Tag>
                              {EXEC_STATUS_TAG[item.execution_status]?.label ?? item.execution_status}
                            </Tag>
                          </Space>
                          <Paragraph style={{ margin: 0, fontSize: 13 }}>{item.copy}</Paragraph>
                          {item.expected_effect ? (
                            <Text type="secondary" style={{ fontSize: 12 }}>
                              预期: {JSON.stringify(item.expected_effect)}
                            </Text>
                          ) : null}
                          {/* 一键执行结果 (200-as-data: blocked 也 200) */}
                          {res ? (
                            res.status === 'executed' ? (
                              <Alert
                                type="success"
                                showIcon
                                message={`已执行${res.task_id ? ` (任务 #${res.task_id})` : ''}${
                                  res.ref_id ? ` 引用 #${res.ref_id}` : ''
                                }`}
                                style={{ marginTop: 4 }}
                              />
                            ) : res.status === 'blocked' ? (
                              <Alert
                                type="warning"
                                showIcon
                                message={`已拦截: ${fmtReason(res.reason)}`}
                                description={res.task_id ? `reach 任务 #${res.task_id}` : undefined}
                                style={{ marginTop: 4 }}
                              />
                            ) : (
                              <Alert
                                type="error"
                                showIcon
                                message={`执行失败: ${fmtReason(res.reason)}`}
                                style={{ marginTop: 4 }}
                              />
                            )
                          ) : null}
                          <div>
                            <Button
                              type="primary"
                              size="small"
                              loading={isExecuting}
                              onClick={() => handleExecute(item)}
                              disabled={item.execution_status === 'executed'}
                            >
                              一键执行
                            </Button>
                          </div>
                        </Space>
                      </List.Item>
                    )
                  }}
                />
              </Card>

              {/* 归因看板 */}
              <Card
                title="归因看板"
                size="small"
                extra={
                  <Button
                    size="small"
                    onClick={() => reloadAttribution(detail.id)}
                    loading={attrLoading}
                  >
                    刷新归因
                  </Button>
                }
              >
                {attrLoading ? (
                  <div style={{ textAlign: 'center', padding: 24 }}>
                    <Spin />
                  </div>
                ) : attribution?.items?.length ? (
                  <Space direction="vertical" style={{ width: '100%' }} size="small">
                    <ReactECharts option={attrOption!} style={{ height: 280 }} />
                    <List
                      size="small"
                      dataSource={attribution.items}
                      renderItem={(it) => (
                        <List.Item>
                          <Space size="small" wrap>
                            <Tag color="orange">#{it.rank}</Tag>
                            <Text>{ACTION_LABEL[it.action_type] ?? it.action_type}</Text>
                            <Text type="secondary">核销率: {fmtPct(it.coupon_redeem)}</Text>
                            <Text type="secondary">成团率: {fmtPct(it.groupbuy_success)}</Text>
                            <Text type="secondary">复购率: {fmtPct(it.repurchase)}</Text>
                            <Text type="secondary">到店增量: {fmtPct(it.visit_uplift)}</Text>
                            <Text type="secondary">
                              增收: {it.revenue_cny != null ? `¥${it.revenue_cny.toFixed(2)}` : '—'}
                            </Text>
                          </Space>
                        </List.Item>
                      )}
                    />
                  </Space>
                ) : (
                  <EmptyState title="暂无归因数据" description="T+24h/T+72h 后回填, 或策略尚未执行" />
                )}
              </Card>
            </Space>
          </div>
        ) : (
          <EmptyState title="加载失败" description="策略详情加载失败, 请重试" />
        )}
      </Drawer>
    </div>
  )
}

/** 0-1 比率 → 百分比字符串; null/undefined → "—" */
function fmtPct(v?: number | null): string {
  if (v == null) return '—'
  return (v * 100).toFixed(1) + '%'
}
