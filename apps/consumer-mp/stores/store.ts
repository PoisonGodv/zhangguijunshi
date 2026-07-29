// stores/store.ts — 轻量响应式 store 内核 (零依赖)
//
// 对齐技术方案 §9.2.4 接口形状: 后续 npm 构建就绪后可平滑替换为
// mobx-miniprogram + mobx-miniprogram-bindings,页面订阅范式不变
// (subscribe(cb) → 取消函数 / get() 取快照 / set(patch) 触发通知)。

export interface Store<T> {
  /** 取当前状态快照 */
  get(): T
  /** 浅合并 patch 并通知所有订阅者 (传新 state) */
  set(patch: Partial<T>): void
  /** 订阅状态变化;返回取消订阅函数 */
  subscribe(cb: (s: T) => void): () => void
}

export function createStore<T>(initial: T): Store<T> {
  let state = initial
  const listeners = new Set<(s: T) => void>()

  return {
    get(): T {
      return state
    },
    set(patch: Partial<T>): void {
      state = { ...state, ...patch }
      // 复制一份再遍历,避免回调内 unsubscribe 影响迭代
      listeners.forEach((cb) => cb(state))
    },
    subscribe(cb: (s: T) => void): () => void {
      listeners.add(cb)
      return () => {
        listeners.delete(cb)
      }
    },
  }
}
