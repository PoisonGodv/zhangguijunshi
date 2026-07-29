// utils/tracker.ts — 业务埋点 (技术方案 §9.2.6)
//
// 自定义事件 + 页面停留。批量入队,>=20 触发 flush;失败/断网 unshift 回队列不丢。
// 后端 /v1/track/batch 待补: 失败回队列是设计行为,不是 bug。

interface TrackEvent {
  name: string
  props: Record<string, unknown>
  ts: number
  sessionId: string
}

// === 全局 globalData (同 utils/request.ts 的 appGlobal 范式;复制避免循环依赖) ===
interface AppGlobalData {
  baseURL: string
  tenantID: number
}

function appGlobal(): AppGlobalData {
  const app = getApp() as unknown as { globalData?: AppGlobalData } | undefined
  if (app && app.globalData && app.globalData.baseURL) return app.globalData
  return { baseURL: 'http://localhost:8080', tenantID: 1 }
}

const FLUSH_THRESHOLD = 20

export class Tracker {
  private queue: TrackEvent[] = []
  private sessionId = `${Date.now()}-${Math.random().toString(36).slice(2)}`
  private pageEnterTs = 0
  private pagePath = ''
  private appVersion = '0.0.1'

  /** 业务事件入队;达到阈值触发批量上报 */
  event(name: string, props: Record<string, unknown> = {}): void {
    this.queue.push({ name, props, ts: Date.now(), sessionId: this.sessionId })
    if (this.queue.length >= FLUSH_THRESHOLD) this.flush()
  }

  /** 页面显示: 记录进入时间与路径 (供 onPageHide 算停留) */
  onPageShow(path: string): void {
    this.pageEnterTs = Date.now()
    this.pagePath = path
  }

  /** 页面隐藏: 计算停留时长,push page_stay 事件 */
  onPageHide(): void {
    if (this.pagePath) {
      const staySec = (Date.now() - this.pageEnterTs) / 1000
      this.event('page_stay', { path: this.pagePath, staySec })
    }
    this.pagePath = ''
  }

  /** 批量上报;失败/断网 unshift 回队列,下次重试 (后端 /v1/track/batch 待补,失败回队列不丢) */
  flush(): void {
    if (!this.queue.length) return
    const batch = this.queue.splice(0, this.queue.length)
    wx.request({
      url: appGlobal().baseURL + '/v1/track/batch',
      method: 'POST',
      timeout: 10000,
      header: { 'Content-Type': 'application/json', 'X-Client': 'consumer-mp' },
      data: { events: batch, appVersion: this.appVersion },
      fail: () => {
        this.queue.unshift(...batch) // 失败回队列,不丢
      },
    })
  }
}

export const tracker = new Tracker()
