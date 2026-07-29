// src/pages/dashboard/index.tsx — 经营看板 (FE-ADMIN-02)
//
// 数据: GET /v1/boss/billing/summary/today + /:date (DailySummary PascalCase)。
// 顶部日期选择 (默认今日) + 4 张 Statistic 卡 (营业额/订单数/实收/客单价)。
// ECharts: ① 热销 TopProducts 条形图 (Name vs GrossCents); ② 近7日营业额趋势线图
//   (循环调 dailySummary(date) 拼数据, 任意一日失败跳过不阻塞)。
// 空态: EmptyState。Money 复用 (分→¥)。
//
// 字段映射 (经 internal/billing/service.go DailySummary 核实):
//   营业额 = GrossCents (Σ order.TotalCents)
//   实收   = NetCents   (Σ order.PaidCents; 已付单 total==paid)
//   客单价 = GrossCents / OrderCount (OrderCount=0 显示 ¥0)
//   热销   = TopProducts[].{Name, GrossCents(=Σ qty*PriceCents), Qty}

import { useCallback, useEffect, useMemo, useState } from 'react'
import { Card, Col, Row, Statistic, DatePicker, Space, Typography } from 'antd'
import dayjs, { type Dayjs } from 'dayjs'
import ReactECharts from 'echarts-for-react'
import Money from '@/components/Money'
import EmptyState from '@/components/EmptyState'
import PageLoading from '@/components/PageLoading'
import { dailySummary, dailySummaryToday, type DailySummary } from '@/services/billing'
import { fmtDate } from '@/utils/format'

const { Title, Text } = Typography

export default function DashboardPage() {
  const [date, setDate] = useState<Dayjs>(dayjs())
  const [summary, setSummary] = useState<DailySummary | null>(null)
  const [loading, setLoading] = useState(false)
  const [trend, setTrend] = useState<{ date: string; gross: number }[]>([])
  const [trendLoading, setTrendLoading] = useState(false)

  // 拉取指定日期流水 (今日走 /today, 其余走 /:date)
  const loadSummary = useCallback(async (d: Dayjs) => {
    setLoading(true)
    try {
      const isToday = d.isSame(dayjs(), 'day')
      const s = isToday ? await dailySummaryToday() : await dailySummary(d.format('YYYY-MM-DD'))
      setSummary(s)
    } catch {
      // request.ts 已 toast 错误; 这里仅清空
      setSummary(null)
    } finally {
      setLoading(false)
    }
  }, [])

  // 拉取近7日趋势 (并发调 /:date, 任意失败跳过)
  const loadTrend = useCallback(async () => {
    setTrendLoading(true)
    const days: Dayjs[] = Array.from({ length: 7 }, (_, i) => dayjs().subtract(6 - i, 'day'))
    const results = await Promise.allSettled(
      days.map((d) => dailySummary(d.format('YYYY-MM-DD')).then((s) => ({
        date: d.format('YYYY-MM-DD'),
        gross: s.GrossCents,
      }))),
    )
    const ok: { date: string; gross: number }[] = []
    results.forEach((r) => {
      if (r.status === 'fulfilled') ok.push(r.value)
    })
    ok.sort((a, b) => (a.date < b.date ? -1 : 1))
    setTrend(ok)
    setTrendLoading(false)
  }, [])

  useEffect(() => {
    void loadSummary(date)
  }, [date, loadSummary])

  useEffect(() => {
    void loadTrend()
  }, [loadTrend])

  const avgCents = useMemo(() => {
    if (!summary || summary.OrderCount === 0) return 0
    return Math.round(summary.GrossCents / summary.OrderCount)
  }, [summary])

  // ECharts: 热销条形图 (横向, 营收分→元)
  const topOption = useMemo(() => {
    const items = (summary?.TopProducts ?? []).slice().reverse() // reverse 让最高的在顶部
    return {
      tooltip: {
        trigger: 'axis',
        axisPointer: { type: 'shadow' },
        formatter: (p: { name: string; value: number }[]) =>
          `${p[0].name}<br/>营收: ¥${(p[0].value).toFixed(2)}`,
      },
      grid: { left: '15%', right: '10%', top: 20, bottom: 20 },
      xAxis: {
        type: 'value',
        name: '营收(元)',
        axisLabel: { formatter: (v: number) => '¥' + v },
      },
      yAxis: {
        type: 'category',
        data: items.map((t) => t.Name),
      },
      series: [
        {
          type: 'bar',
          data: items.map((t) => t.GrossCents / 100),
          itemStyle: { color: '#FF6B35' },
          barMaxWidth: 24,
        },
      ],
    }
  }, [summary])

  // ECharts: 近7日营业额趋势线图
  const trendOption = useMemo(() => {
    return {
      tooltip: { trigger: 'axis' },
      grid: { left: '12%', right: '8%', top: 20, bottom: 30 },
      xAxis: {
        type: 'category',
        data: trend.map((t) => fmtDate(t.date)),
        axisLabel: { formatter: (v: string) => v.slice(5) }, // MM-DD
      },
      yAxis: {
        type: 'value',
        name: '营业额(元)',
        axisLabel: { formatter: (v: number) => '¥' + v },
      },
      series: [
        {
          type: 'line',
          data: trend.map((t) => t.gross / 100),
          smooth: true,
          itemStyle: { color: '#FF6B35' },
          areaStyle: { opacity: 0.15 },
        },
      ],
    }
  }, [trend])

  if (loading && !summary) {
    return <PageLoading tip="加载当日流水..." />
  }

  return (
    <div>
      <Space style={{ marginBottom: 16 }} align="center">
        <Title level={4} style={{ margin: 0 }}>经营看板</Title>
        <DatePicker
          value={date}
          onChange={(d) => d && setDate(d)}
          allowClear={false}
          disabledDate={(current) => current && current.isAfter(dayjs(), 'day')}
        />
      </Space>

      <Row gutter={16} style={{ marginBottom: 16 }}>
        <Col span={6}>
          <Card>
            <Statistic
              title="今日营业额"
              prefix="¥"
              value={summary ? summary.GrossCents / 100 : 0}
              precision={2}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic title="今日订单数" value={summary?.OrderCount ?? 0} />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <Statistic
              title="今日实收"
              prefix="¥"
              value={summary ? summary.NetCents / 100 : 0}
              precision={2}
            />
          </Card>
        </Col>
        <Col span={6}>
          <Card>
            <div style={{ fontSize: 14, color: 'rgba(0,0,0,0.45)', marginBottom: 8 }}>客单价</div>
            <Money value={avgCents} style={{ fontSize: 24, fontWeight: 500 }} colorSign={false} />
          </Card>
        </Col>
      </Row>

      <Row gutter={16}>
        <Col span={12}>
          <Card title="热销商品 Top5" extra={<Text type="secondary">按销量</Text>}>
            {summary && summary.TopProducts && summary.TopProducts.length > 0 ? (
              <ReactECharts option={topOption} style={{ height: 320 }} />
            ) : (
              <EmptyState title="暂无热销数据" description="当日无已付订单" />
            )}
          </Card>
        </Col>
        <Col span={12}>
          <Card
            title="近7日营业额趋势"
            extra={
              trend.length < 7 ? (
                <Text type="secondary">部分日期无数据</Text>
              ) : null
            }
          >
            {trend.length > 0 ? (
              <ReactECharts option={trendOption} style={{ height: 320 }} />
            ) : (
              <EmptyState title="暂无趋势数据" description="近7日无已付订单" />
            )}
          </Card>
        </Col>
      </Row>

      {trendLoading && (
        <div style={{ marginTop: 16, textAlign: 'center' }}>
          <Text type="secondary">趋势数据加载中...</Text>
        </div>
      )}
    </div>
  )
}
