"use client"

import { useEffect, useMemo, useRef, useState } from "react"
import { apiFetch } from "@/lib/api"
import { useRefreshTick } from "@/lib/refresh-context"
import type {
  AppVersion,
  BalanceTrendPoint,
  CaptchaConfig,
  Channel,
  ChannelListOrder,
  ChannelListSort,
  ChannelListStatus,
  ChannelPage,
  ConnectorPlugin,
  CostTrendPoint,
  DashboardSummary,
  GatewayUsageStats,
  NotificationChannel,
  NotificationLogPage,
  ProfitConfig,
  ProfitSummary,
  RateChangeLogPage,
  RateSnapshot,
  SystemConfigResponse,
  UpstreamAnnouncementPage,
} from "@/lib/api-types"

export interface QueryState<T> {
  data: T | null
  loading: boolean
  error: string | null
  /** data 是用哪个 path 拉到的；还没拿到过数据时为 null。 */
  dataPath: string | null
  /**
   * path 已经变了、新 path 的数据还没到，data 仍是旧 path 的结果。
   * 同一 path 的轮询 / refetch 不算 stale；需要区分"旧条件的数据"时用它（例如筛选条件切换）。
   */
  stale: boolean
  refetch: () => void
  setData: (data: T) => void
}

/**
 * In-flight 请求去重：同一个 URL 在同一个 tick 内只发一次，所有 useApi 共享 Promise。
 *
 * 为什么需要：useDashboardSummary() 在 5 个组件里都被调用，没去重的话每次 mount /
 * refresh 都会发 5 个相同请求。开发环境叠加 StrictMode 翻倍后会更夸张。
 */
const inflight = new Map<string, Promise<unknown>>()

/** Cache 已完成的响应一小段时间，便于同一帧内挂载的多个组件共享结果（即使第一次的 Promise 已经 resolve）。 */
interface CacheEntry {
  data: unknown
  expiresAt: number
}
const cache = new Map<string, CacheEntry>()
const CACHE_TTL_MS = 800

/**
 * cache 的硬上限。
 *
 * key 里带了 tick / bump，每次全局刷新（30s 一次，或点任意按钮触发 refresh()）都会换一批新 key，
 * 旧 key 的条目永远不会再被命中。原先只 set 不 delete，等于每 30 秒往 Map 里塞一批死数据：
 * 渠道卡片的倍率请求按渠道数增长，页面挂几小时就是几千上万条，内存单调上涨直到标签页卡死。
 * 这里既清过期项，也在超限时按插入顺序淘汰最老的条目。
 */
const CACHE_MAX_ENTRIES = 200

function cacheKey(path: string, tick: number, bump: number) {
  return `${path}#${tick}#${bump}`
}

function storeCache(key: string, data: unknown) {
  cache.set(key, { data, expiresAt: Date.now() + CACHE_TTL_MS })
  if (cache.size <= CACHE_MAX_ENTRIES) return

  const now = Date.now()
  for (const [k, v] of cache) {
    if (v.expiresAt <= now) cache.delete(k)
  }
  // Map 保持插入顺序，从最老的开始丢。
  while (cache.size > CACHE_MAX_ENTRIES) {
    const oldest = cache.keys().next()
    if (oldest.done) break
    cache.delete(oldest.value)
  }
}

function fetchShared<T>(path: string, key: string): Promise<T> {
  const now = Date.now()

  const cached = cache.get(key)
  if (cached && cached.expiresAt > now) {
    return Promise.resolve(cached.data as T)
  }

  const existing = inflight.get(key) as Promise<T> | undefined
  if (existing) return existing

  const p = apiFetch<T>(path)
    .then((d) => {
      storeCache(key, d)
      return d
    })
    .finally(() => {
      // 让下一帧（refresh tick++）拉到新的数据，不要永远 hold 住旧 promise
      inflight.delete(key)
    })
  inflight.set(key, p)
  return p
}

/**
 * useApi 通用数据获取 hook（stale-while-revalidate）。
 * - 首次加载：loading = true，组件显示加载占位
 * - 后续刷新（refresh tick / refetch）：保留旧 data 继续展示，loading 不切回 true，后台静默拉新
 * - path 变化时同样保留旧 data，新数据到达前 stale = true，调用方可据此区分新旧条件的结果
 * - 同 URL + 同 tick 的并发调用共享一次请求
 */
export function useApi<T>(path: string | null, watchRefresh = true): QueryState<T> {
  const [data, setData] = useState<T | null>(null)
  const [dataPath, setDataPath] = useState<string | null>(null)
  const [loading, setLoading] = useState<boolean>(path !== null)
  const [error, setError] = useState<string | null>(null)
  const [bump, setBump] = useState(0)
  const refreshTick = useRefreshTick()
  const globalTick = watchRefresh ? refreshTick : 0

  // 已经拿到过数据吗？用 ref 防止 setLoading 写回触发额外 effect。
  const hasDataRef = useRef(false)

  useEffect(() => {
    if (path === null) {
      setLoading(false)
      return
    }
    let cancelled = false
    // 关键：只有第一次（还没拿到过数据）才展示 loading；后续 polling / refetch 静默进行，
    // 避免组件因 loading=true 短暂消失再回来造成"闪屏"。
    if (!hasDataRef.current) setLoading(true)
    setError(null)
    fetchShared<T>(path, cacheKey(path, globalTick, bump))
      .then((d) => {
        if (cancelled) return
        hasDataRef.current = true
        setData(d)
        setDataPath(path)
      })
      .catch((e: Error) => {
        if (cancelled) return
        setError(e.message)
      })
      .finally(() => {
        if (cancelled) return
        setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [path, bump, globalTick])

  return {
    data,
    loading,
    error,
    dataPath,
    stale: dataPath !== null && dataPath !== path,
    refetch: () => setBump((b) => b + 1),
    setData: (nextData) => {
      hasDataRef.current = true
      setData(nextData)
      setDataPath(path)
    },
  }
}

export function useDashboardSummary() {
  return useApi<DashboardSummary>("/dashboard/summary")
}

/** 本地自然日起止（RFC3339），用于网关用量「今日」统计 */
function localDayRangeISO(day = new Date()): { from: string; to: string } {
  const start = new Date(day.getFullYear(), day.getMonth(), day.getDate(), 0, 0, 0, 0)
  const end = new Date(day.getFullYear(), day.getMonth(), day.getDate(), 23, 59, 59, 999)
  return { from: start.toISOString(), to: end.toISOString() }
}

/** 网关使用记录聚合统计（默认今日本地时区） */
export function useGatewayUsageStatsToday() {
  const { from, to } = localDayRangeISO()
  const qs = new URLSearchParams({ from, to })
  return useApi<GatewayUsageStats>(`/gateway/usage/stats?${qs}`)
}

export function useAppVersion() {
  return useApi<AppVersion>("/version", false)
}

export function useBalanceTrend(days = 7) {
  return useApi<BalanceTrendPoint[]>(`/dashboard/balance-trend?days=${days}`)
}

export function useCostTrend(days = 7) {
  return useApi<CostTrendPoint[]>(`/dashboard/cost-trend?days=${days}`)
}

/** 编译期注册的渠道插件清单（含能力位与 token 字段声明）。 */
export function useConnectorPlugins() {
  return useApi<ConnectorPlugin[]>("/connector-plugins")
}

export function useChannels() {
  return useApi<Channel[]>("/channels")
}

/** 渠道分页列表的搜索 / 筛选 / 排序条件，空值不会拼进查询串 */
export interface ChannelPageFilter {
  q?: string
  status?: ChannelListStatus
  tag?: string
  sort?: ChannelListSort
  order?: ChannelListOrder
}

// 固定参数顺序，保证同样的条件得到同样的 URL，便于 fetchShared 去重。
const channelPageFilterKeys = ["q", "status", "tag", "sort", "order"] as const

// total / pages 只取决于这几个参数；翻页、改排序时旧结果里的计数仍然准确。
const channelPageCountKeys = ["page_size", "q", "status", "tag"] as const

function channelPageCountsKey(path: string): string {
  const qs = new URLSearchParams(path.slice(path.indexOf("?") + 1))
  return channelPageCountKeys.map((key) => `${key}=${qs.get(key) ?? ""}`).join("&")
}

export interface ChannelPageQueryState extends QueryState<ChannelPage> {
  /**
   * data 的 total / pages 属于旧的筛选条件或每页数量（新结果还没到）：
   * 此时不要展示筛选数量、页码或"没有匹配"，旧列表只能作为占位。翻页、改排序不会置为 true。
   */
  countsStale: boolean
}

export function useChannelsPage(
  page = 1,
  pageSize = 9,
  filter: ChannelPageFilter = {},
): ChannelPageQueryState {
  const qs = new URLSearchParams()
  qs.set("page", String(page))
  qs.set("page_size", String(pageSize))
  for (const key of channelPageFilterKeys) {
    const value = filter[key]?.trim()
    if (value) qs.set(key, value)
  }
  const path = `/channels?${qs.toString()}`
  const query = useApi<ChannelPage>(path)
  return {
    ...query,
    countsStale:
      query.dataPath !== null &&
      channelPageCountsKey(query.dataPath) !== channelPageCountsKey(path),
  }
}

export function useChannelRates(channelID: number | null, onlyWithKeys = false) {
  const path =
    channelID == null
      ? null
      : onlyWithKeys
        ? `/channels/${channelID}/rates?only_with_keys=1`
        : `/channels/${channelID}/rates`
  return useApi<RateSnapshot[]>(path)
}

/** 倍率空结果的稳定引用：避免每次渲染都新建 [] 让 memo 组件白重渲染。 */
export const EMPTY_RATES: RateSnapshot[] = []

export interface ChannelRatesBatchState {
  /** channelID → 该渠道的倍率快照；没有数据的渠道返回 EMPTY_RATES。 */
  rates: Record<number, RateSnapshot[]>
  loading: boolean
  error: string | null
}

/**
 * useChannelRatesBatch 一次拉回多个渠道的倍率快照（/rates/batch）。
 *
 * 渠道卡片网格原先给每张卡片各挂一个 useChannelRates：一屏 N 张卡片 = N 个请求，
 * 每 30s 刷新 tick 再全部重发一遍；N 大时浏览器连接池被打满，主线程光是排队解析响应就卡。
 * 现在整个网格只发一个请求。
 *
 * channelIDs 按升序排序后再拼进 URL，保证同一批渠道得到同一个 path，能命中 fetchShared 去重。
 * onlyWithKeysIDs 是"仅显示已创建密钥的分组"开关打开的渠道，语义与单渠道接口的 ?only_with_keys=1 一致。
 */
export function useChannelRatesBatch(
  channelIDs: number[],
  onlyWithKeysIDs: number[] = [],
): ChannelRatesBatchState {
  const idsKey = channelIDs.slice().sort((a, b) => a - b).join(",")
  const keysKey = onlyWithKeysIDs
    .filter((id) => channelIDs.includes(id))
    .slice()
    .sort((a, b) => a - b)
    .join(",")
  const path =
    idsKey === ""
      ? null
      : `/rates/batch?ids=${idsKey}${keysKey ? `&only_with_keys=${keysKey}` : ""}`
  const query = useApi<Record<string, RateSnapshot[]>>(path)
  const data = query.data

  const rates = useMemo(() => {
    const out: Record<number, RateSnapshot[]> = {}
    for (const id of channelIDs) {
      const list = data?.[String(id)]
      out[id] = list && list.length > 0 ? list : EMPTY_RATES
    }
    return out
    // idsKey 已覆盖 channelIDs 的内容；data 引用变化才需要重建
  }, [data, idsKey])

  return { rates, loading: query.loading, error: query.error }
}

// useMultiChannelRates 把多个上游渠道的倍率分组拉回来合并去重，
// 供订阅规则"多选渠道 + 指定分组"场景使用。复用 fetchShared 缓存，
// 单渠道请求仍与 useChannelRates 共享，不会重复打接口。
export function useMultiChannelRates(channelIDs: number[]) {
  const [data, setData] = useState<RateSnapshot[] | null>(null)
  const [loading, setLoading] = useState(false)
  const [bump, setBump] = useState(0)
  const refreshTick = useRefreshTick()
  const key = channelIDs.slice().sort((a, b) => a - b).join(",")

  useEffect(() => {
    if (channelIDs.length === 0) {
      setData(null)
      setLoading(false)
      return
    }
    let cancelled = false
    setLoading(true)
    Promise.all(
      channelIDs.map((id) =>
        fetchShared<RateSnapshot[]>(
          `/channels/${id}/rates`,
          cacheKey(`/channels/${id}/rates`, refreshTick, bump),
        ),
      ),
    )
      .then((results) => {
        if (cancelled) return
        setData(results.flat())
      })
      .catch(() => {
        if (!cancelled) setData(null)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
    // channelIDs 是数组引用，用排序后的 key 字符串做依赖避免每次渲染都触发
  }, [key, refreshTick, bump])

  return { data, loading, refetch: () => setBump((b) => b + 1) }
}

export function useRateChanges(page = 1, pageSize = 20, channelID?: number) {
  const q = new URLSearchParams()
  q.set("page", String(page))
  q.set("page_size", String(pageSize))
  if (channelID != null) q.set("channel_id", String(channelID))
  return useApi<RateChangeLogPage>(`/rate-changes?${q.toString()}`)
}

export function useNotificationChannels() {
  return useApi<NotificationChannel[]>("/notifications/channels")
}

export function useNotificationLogs(page = 1, pageSize = 20) {
  return useApi<NotificationLogPage>(
    `/notifications/logs?page=${page}&page_size=${pageSize}`,
  )
}

export function useAnnouncements(page = 1, pageSize = 20) {
  return useApi<UpstreamAnnouncementPage>(
    `/announcements?page=${page}&page_size=${pageSize}`,
  )
}

export function useCaptchaConfigs(enabled = true) {
  return useApi<CaptchaConfig[]>(enabled ? "/captcha-configs" : null)
}

export function useSystemConfig() {
  return useApi<SystemConfigResponse>("/settings/config")
}

/**
 * 今日收益结果。后端不做实时计算，这里只读当天最新一条快照，
 * 所以「更新」按钮才会去触发 /api/profit/refresh。
 */
export function useProfitSummary() {
  return useApi<ProfitSummary>("/profit/summary")
}

/** 收益统计的全局开关与自有站点列表（凭据只回 has_token / has_cookie）。 */
export function useProfitConfig() {
  return useApi<ProfitConfig>("/profit/config")
}
