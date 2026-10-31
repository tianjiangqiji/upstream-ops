/**
 * SSE 流读取：用 fetch 而非 EventSource，以便附带 Authorization Bearer 头。
 */

import { getToken, notifyUnauthorized } from "@/lib/api"

export type ProgressStage =
  | "revenue"
  | "admin_cost"
  | "captcha"
  | "session"
  | "login"
  | "balance"
  | "cost"
  | "subscription"
  | "rates"
  | "done"
  | "error"

export interface ProgressEvent {
  stage: ProgressStage
  message: string
  ok?: boolean
  data?: unknown
  time: string
  channel_id?: number
  channel_name?: string
  index?: number
  total?: number
}

export interface SyncOptions {
  onEvent: (ev: ProgressEvent) => void
  signal?: AbortSignal
  /** 可选 JSON 请求体，例如收益重试只带需要重试的渠道 id。 */
  body?: unknown
}

/**
 * streamSSE 通用 SSE 读取器：POST 指定 path，逐条消费 data 帧。
 * 401 → 触发全局登出回调；其他非 2xx 抛错。
 *
 * 关于取消：调用方传进来的 signal 只 abort 掉 fetch 本身，底层 reader 不会自动释放，
 * 服务端还在往外写、浏览器还在收，主线程就一直有帧要解析（批量同步 N 个渠道时尤其明显）。
 * 这里额外持有一个内部 AbortController：外部 abort 时一并触发，finally 里再 reader.cancel()，
 * 保证流真正断掉。
 */
async function streamSSE(
  path: string,
  { onEvent, signal, body }: SyncOptions,
): Promise<void> {
  const token = getToken()
  const controller = new AbortController()
  const onExternalAbort = () => controller.abort(signal?.reason)
  if (signal) {
    if (signal.aborted) controller.abort(signal.reason)
    else signal.addEventListener("abort", onExternalAbort, { once: true })
  }

  let reader: ReadableStreamDefaultReader<Uint8Array> | null = null
  try {
    const res = await fetch(path, {
      method: "POST",
      headers: {
        Accept: "text/event-stream",
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        ...(body !== undefined ? { "Content-Type": "application/json" } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
      signal: controller.signal,
    })

    if (res.status === 401) {
      notifyUnauthorized()
      throw new Error("未登录或会话已过期")
    }
    if (!res.ok) {
      const body = await res.text().catch(() => "")
      throw new Error(`${path} 请求失败 (${res.status}): ${body || res.statusText}`)
    }
    if (!res.body) {
      throw new Error("浏览器不支持流式响应")
    }

    reader = res.body.getReader()
    const decoder = new TextDecoder("utf-8")
    let buf = ""

    while (true) {
      const { value, done } = await reader.read()
      if (done) {
        if (buf.trim()) flushBlock(buf, onEvent)
        return
      }
      buf += decoder.decode(value, { stream: true })
      let idx
      while ((idx = buf.indexOf("\n\n")) >= 0) {
        const block = buf.slice(0, idx)
        buf = buf.slice(idx + 2)
        flushBlock(block, onEvent)
      }
    }
  } finally {
    if (signal) signal.removeEventListener("abort", onExternalAbort)
    if (reader) {
      // 先 cancel 再 releaseLock：只 releaseLock 的话底层流还活着，后续帧仍会到达。
      await reader.cancel().catch(() => undefined)
      reader.releaseLock()
    }
  }
}

function flushBlock(block: string, onEvent: (ev: ProgressEvent) => void) {
  const lines = block.split("\n")
  const dataLines: string[] = []
  for (const line of lines) {
    if (line.startsWith("data:")) {
      dataLines.push(line.slice(5).trimStart())
    }
  }
  if (dataLines.length === 0) return
  const payload = dataLines.join("\n")
  try {
    const ev = JSON.parse(payload) as ProgressEvent
    onEvent(ev)
  } catch {
    // 忽略不合法的帧
  }
}

/** 触发 /api/channels/:id/sync（余额 + 倍率）。 */
export function syncChannelStream(channelID: number, options: SyncOptions) {
  return streamSSE(`/api/channels/${channelID}/sync`, options)
}

/** 触发 /api/channels/sync-all（余额 + 消费，逐渠道串行）。 */
export function syncAllChannelsStream(options: SyncOptions) {
  return streamSSE("/api/channels/sync-all", options)
}

/** 触发 /api/channels/:id/test-login（仅登录验证，session 落库以便后续 sync 复用）。 */
export function testLoginStream(channelID: number, options: SyncOptions) {
  return streamSSE(`/api/channels/${channelID}/test-login`, options)
}

/**
 * 触发 /api/profit/refresh：完整跑一轮今日收益。
 * 流程是 主站总收益 → 管理员消费 → 逐供应商今日消费，每一步都会推事件。
 */
export function refreshProfitStream(options: SyncOptions) {
  return streamSSE("/api/profit/refresh", options)
}

/** 触发 /api/profit/retry：只重试指定供应商，主站金额沿用上一轮快照。 */
export function retryProfitStream(channelIDs: number[], options: SyncOptions) {
  return streamSSE("/api/profit/retry", {
    ...options,
    body: { channel_ids: channelIDs },
  })
}
