"use client"

import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import {
  AlertTriangle,
  CheckCircle2,
  Info,
  Loader2,
  Plus,
  RefreshCw,
  Settings2,
  TrendingUp,
  XCircle,
} from "lucide-react"

import { ChannelFormDialog } from "@/components/monitor/channel-form-dialog"
import { ProfitSiteDialog } from "@/components/monitor/profit-site-dialog"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Progress } from "@/components/ui/progress"
import { money, relativeTime } from "@/lib/format"
import { useChannels, useProfitConfig, useProfitSummary } from "@/lib/queries"
import { useTriggerRefresh } from "@/lib/refresh-context"
import { refreshProfitStream, retryProfitStream } from "@/lib/sync-stream"
import { cn } from "@/lib/utils"
import type { Channel, ProfitSite, ProfitSiteProfit, ProfitSummary } from "@/lib/api-types"
import type { ProgressEvent, ProgressStage } from "@/lib/sync-stream"

const stageLabel: Record<ProgressStage, string> = {
  revenue: "总收益",
  admin_cost: "管理员",
  captcha: "打码",
  session: "会话",
  login: "登录",
  balance: "余额",
  cost: "消费",
  subscription: "订阅",
  rates: "倍率",
  done: "完成",
  error: "失败",
}

interface ProfitStep {
  key: string
  label: string
  event: ProgressEvent
}

interface ProfitEventEntry {
  event: ProgressEvent
  label: string
}

/**
 * 步骤归并键：站点步骤靠 site_id 区分（多个站点各两条），
 * 供应商消费步骤靠 channel_id 区分，流程级事件两者都没有。
 */
function stepKey(ev: ProgressEvent): string {
  return `${ev.stage}#${ev.site_id ?? 0}#${ev.channel_id ?? 0}`
}

function isFinished(ev: ProgressEvent): boolean {
  return ev.ok === true || ev.ok === false || ev.stage === "done" || ev.stage === "error"
}

/** 渠道消费步骤用渠道名当标签，站点步骤用「站点名·阶段」，流程级事件用固定标签。 */
function stepLabel(ev: ProgressEvent): string {
  if (ev.channel_id != null && ev.stage === "cost") {
    return ev.channel_name?.trim() || `#${ev.channel_id}`
  }
  const stage = stageLabel[ev.stage] ?? "进度"
  const siteName = ev.site_name?.trim()
  if (siteName) return `${siteName}·${stage}`
  return stage
}

interface ProfitDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 打开即自动跑一轮更新；只看进度/结果时可以传 false。 */
  autoStart?: boolean
}

/**
 * 「今日收益」更新弹窗。
 *
 * 一轮更新的完整过程都在这里可见：先逐站点拉总收益与管理员测试消费，
 * 再逐个供应商重新采集今日消费并累减。采集失败的站点或供应商都不会让流程中断，
 * 而是沿用上一次成功的金额计入，同时列在下方供用户重试，
 * 或直接打开该渠道的设置修正凭据后再重试。
 */
export function ProfitDialog({ open, onOpenChange, autoStart = true }: ProfitDialogProps) {
  const summaryQuery = useProfitSummary()
  const configQuery = useProfitConfig()
  const channelsQuery = useChannels()
  const refresh = useTriggerRefresh()

  const [summary, setSummary] = useState<ProfitSummary | null>(null)
  const [steps, setSteps] = useState<Record<string, ProfitEventEntry>>({})
  const [order, setOrder] = useState<string[]>([])
  const [running, setRunning] = useState(false)
  const [retrying, setRetrying] = useState(false)
  const [fatal, setFatal] = useState<string | null>(null)
  const [channelTotal, setChannelTotal] = useState(0)
  const [editingChannel, setEditingChannel] = useState<Channel | null>(null)
  const [siteDialogOpen, setSiteDialogOpen] = useState(false)
  const [editingSite, setEditingSite] = useState<ProfitSite | null>(null)

  const streamRef = useRef<AbortController | null>(null)
  // 每次打开只自动跑一轮，避免轮询把 SSE 反复触发。
  const autoStartedRef = useRef(false)

  const configured = summary?.configured ?? false
  const enabled = summary?.enabled ?? true
  const hasSnapshot = summary?.has_snapshot ?? false
  const siteCount = summary?.site_count ?? 0

  const applyEvent = useCallback((ev: ProgressEvent) => {
    if (ev.total != null) setChannelTotal(ev.total)
    const payload = ev.data as { summary?: ProfitSummary } | undefined
    if (payload?.summary) setSummary(payload.summary)
    // 没有 channel_id / site_id 的 error 是流程级结论（含"计算完成但有站点或渠道失败"），
    // 只在顶部提示条展示，不进步骤列表，避免同一条消息出现两次。
    if (ev.stage === "error" && ev.channel_id == null && ev.site_id == null) {
      setFatal(ev.message)
      return
    }
    const key = stepKey(ev)
    setSteps((prev) => ({ ...prev, [key]: { event: ev, label: stepLabel(ev) } }))
    setOrder((prev) => (prev.includes(key) ? prev : [...prev, key]))
  }, [])

  const abort = useCallback(() => {
    streamRef.current?.abort()
    streamRef.current = null
  }, [])

  const run = useCallback(
    async (mode: "refresh" | "retry", channelIDs: number[] = []) => {
      abort()
      const controller = new AbortController()
      streamRef.current = controller
      setSteps({})
      setOrder([])
      setFatal(null)
      setChannelTotal(0)
      setRunning(true)
      setRetrying(mode === "retry")
      try {
        const options = {
          signal: controller.signal,
          onEvent: (ev: ProgressEvent) => applyEvent(ev),
        }
        if (mode === "retry") {
          await retryProfitStream(channelIDs, options)
        } else {
          await refreshProfitStream(options)
        }
        // 兜底：done 事件没带 summary（或中途失败）时以服务端快照为准。
        void summaryQuery.refetch()
      } catch (e) {
        if (controller.signal.aborted) return
        setFatal((e as Error).message || "收益更新失败")
      } finally {
        if (streamRef.current === controller) streamRef.current = null
        setRunning(false)
        setRetrying(false)
        refresh()
      }
    },
    [abort, applyEvent, refresh, summaryQuery],
  )

  // 弹窗打开时用最近一次快照先填充结果区；关闭时中断还在跑的流。
  useEffect(() => {
    if (!open) {
      abort()
      autoStartedRef.current = false
      return
    }
    if (summaryQuery.data) setSummary(summaryQuery.data)
  }, [open, abort, summaryQuery.data])

  // 配置好后打开弹窗即自动更新一轮。
  useEffect(() => {
    if (!open || !autoStart || autoStartedRef.current) return
    const data = summaryQuery.data
    if (data == null || !data.configured || !data.enabled) return
    autoStartedRef.current = true
    void run("refresh")
  }, [open, autoStart, summaryQuery.data, run])

  useEffect(() => () => abort(), [abort])

  // 只有 fallback / missing 才算失败；carried 是本轮没参与采集的渠道，不算。
  const failedChannels = useMemo(
    () => (summary?.channels ?? []).filter((c) => c.source === "fallback" || c.source === "missing"),
    [summary],
  )

  // 只有「本轮真的算过、但金额没取到」才算失败；sampled=false 是快照里还没这个站点的
  // 明细（刚添加 / 刚从旧版升级），属于还没跑，不能当成采集失败去让用户改凭据。
  const failedSites = useMemo(() => {
    return (summary?.sites ?? []).filter(
      (s) => s.sampled && (s.error != null || !s.revenue_ok || !s.admin_ok),
    )
  }, [summary])

  const pendingSites = useMemo(() => {
    return (summary?.sites ?? []).filter((s) => !s.sampled)
  }, [summary])

  const channelByID = useMemo(() => {
    const map = new Map<number, Channel>()
    for (const c of channelsQuery.data ?? []) map.set(c.id, c)
    return map
  }, [channelsQuery.data])

  const stepList = useMemo<ProfitStep[]>(() => {
    return order
      .map((key) => {
        const entry = steps[key]
        return entry ? { key, label: entry.label, event: entry.event } : null
      })
      .filter((v): v is ProfitStep => v != null)
  }, [order, steps])

  const finishedCount = useMemo(
    () => stepList.filter((s) => isFinished(s.event)).length,
    [stepList],
  )
  // 每个站点两条（总收益 + 管理员消费），每个渠道一条，外加流程结束事件。
  const expectedSteps = Math.max(1 + siteCount * 2 + channelTotal, finishedCount, 1)
  const percent = Math.min(100, Math.round((finishedCount / expectedSteps) * 100))

  const handleRetry = useCallback(() => {
    const ids = failedChannels.map((c) => c.channel_id)
    if (ids.length === 0) return
    void run("retry", ids)
  }, [failedChannels, run])

  /** 站点失败时直接打开该站点的编辑弹窗，改完凭据保存即可重新跑。 */
  const openSiteEditor = useCallback(
    (siteID: number) => {
      const site = (configQuery.data?.sites ?? []).find((s) => s.id === siteID) ?? null
      setEditingSite(site)
      setSiteDialogOpen(true)
    },
    [configQuery.data],
  )

  const handleSiteSaved = useCallback(() => {
    void configQuery.refetch()
    void summaryQuery.refetch()
    // 刚加的站点默认启用，保存后立刻跑一轮，用户不用再点一次。
    void run("refresh")
  }, [configQuery, summaryQuery, run])

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="sm:max-w-2xl"
        onInteractOutside={(event) => {
          if (editingChannel != null) event.preventDefault()
        }}
        onEscapeKeyDown={(event) => {
          if (editingChannel != null) event.preventDefault()
        }}
      >
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-base font-medium">
            <TrendingUp className="size-4 text-success" />
            {"今日收益"}
          </DialogTitle>
          <DialogDescription className="text-xs">
            {running
              ? retrying
                ? "正在重试失败的供应商…"
                : "正在逐站点获取收益并采集供应商消费…"
              : hasSnapshot
                ? `上次更新于 ${relativeTime(summary?.sampled_at)}，收益不是实时数据`
                : "尚未更新过，点击下方按钮开始"}
          </DialogDescription>
        </DialogHeader>

        {!configured ? (
          <div className="space-y-3">
            <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning">
              <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
              <span>
                {"收益统计需要先添加你自己的 NewAPI 站点：站点总收益取该站日志统计，管理员消费取该站个人日志统计。可以添加多个站点，分别统计。"}
              </span>
            </div>
            <div className="flex justify-end">
              <Button
                type="button"
                size="sm"
                className="gap-1"
                onClick={() => {
                  setEditingSite(null)
                  setSiteDialogOpen(true)
                }}
              >
                <Plus className="size-3.5" />
                {"添加站点"}
              </Button>
            </div>
          </div>
        ) : (
          <div className="min-w-0 space-y-3">
            <div className="space-y-1.5">
              <div className="flex items-center justify-between text-xs text-muted-foreground">
                <span>
                  {running
                    ? `已完成 ${finishedCount} / ${expectedSteps} 步`
                    : fatal
                      ? finishedCount > 0
                        ? "已完成，部分站点或供应商采集失败"
                        : "无法开始"
                      : finishedCount > 0
                        ? "本轮更新已结束"
                        : "等待开始"}
                </span>
                {running ? <span className="tabular-nums">{percent}%</span> : null}
              </div>
              <Progress value={percent} />
            </div>

            <div className="max-h-[38vh] overflow-y-auto rounded-lg border border-border bg-muted/30 px-3 py-2.5">
              {stepList.length === 0 ? (
                <div className="flex items-center gap-2 text-xs">
                  <Loader2 className="size-3.5 shrink-0 animate-spin text-muted-foreground" />
                  <span className="text-foreground/80">{"准备中…"}</span>
                </div>
              ) : (
                <ul className="space-y-1.5">
                  {stepList.map((step) => {
                    const ev = step.event
                    const failed = ev.stage === "error" || ev.ok === false
                    const succeeded = ev.stage === "done" || ev.ok === true
                    const pending = !failed && !succeeded
                    const Icon = pending ? Loader2 : failed ? XCircle : CheckCircle2
                    const tone = pending
                      ? "text-muted-foreground"
                      : failed
                        ? "text-danger"
                        : "text-success"
                    return (
                      <li
                        key={step.key}
                        className="flex items-start gap-2 text-xs animate-in fade-in duration-200"
                      >
                        <Icon
                          className={cn("size-3.5 shrink-0", tone, pending && "animate-spin")}
                        />
                        <span className="w-24 shrink-0 text-[11px] text-muted-foreground [overflow-wrap:anywhere]">
                          {step.label}
                        </span>
                        <span
                          className={cn(
                            "block min-w-0 flex-1 whitespace-pre-wrap [overflow-wrap:anywhere]",
                            failed
                              ? "text-danger"
                              : pending
                                ? "text-foreground/80"
                                : "text-foreground",
                          )}
                        >
                          {ev.message}
                        </span>
                      </li>
                    )
                  })}
                </ul>
              )}
            </div>

            {!enabled ? (
              <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning">
                <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                <span>{"收益统计已在系统设置里关闭，打开「启用收益统计」后才能发起更新。"}</span>
              </div>
            ) : null}

            {fatal ? (
              <div className="flex items-start gap-2 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">
                <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                <span className="min-w-0 flex-1 whitespace-pre-wrap [overflow-wrap:anywhere]">{fatal}</span>
              </div>
            ) : null}

            {hasSnapshot && summary ? <ProfitResultPanel summary={summary} /> : null}

            {failedSites.length > 0 ? (
              <div className="space-y-2 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2.5">
                <div className="flex items-start gap-2 text-xs text-warning">
                  <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                  <span>
                    {`${failedSites.length} 个站点采集失败，已按上次成功的金额计入。可以打开系统设置修正凭据后再更新。`}
                  </span>
                </div>
                <ul className="space-y-1">
                  {failedSites.map((s) => (
                    <li
                      key={s.site_id}
                      className="flex flex-wrap items-center justify-between gap-2 rounded-md bg-background/60 px-2 py-1.5 text-xs"
                    >
                      <span className="flex min-w-0 max-w-full items-center gap-2">
                        <span className="truncate font-medium text-foreground">{s.site_name}</span>
                        <span className="shrink-0 text-muted-foreground">
                          {siteFailReason(s)}
                        </span>
                      </span>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        className="h-7 gap-1 px-2 text-xs"
                        onClick={() => openSiteEditor(s.site_id)}
                      >
                        <Settings2 className="size-3.5" />
                        {"修正凭据"}
                      </Button>
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}

            {pendingSites.length > 0 ? (
              <div className="flex items-start gap-2 rounded-lg border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
                <Info className="mt-0.5 size-3.5 shrink-0" />
                <span className="min-w-0">
                  {`${pendingSites.map((s) => s.site_name).join("、")} 还没有收益明细（刚添加或刚升级），再更新一次就会计入。`}
                </span>
              </div>
            ) : null}

            {failedChannels.length > 0 ? (
              <div className="space-y-2 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2.5">
                <div className="flex items-start gap-2 text-xs text-warning">
                  <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                  <span>
                    {`${failedChannels.length} 个供应商采集失败，已按上次成功的金额计入。可以重试，或打开设置修正凭据后再重试。`}
                  </span>
                </div>
                <ul className="space-y-1">
                  {failedChannels.map((c) => {
                    const channel = channelByID.get(c.channel_id)
                    return (
                      <li
                        key={c.channel_id}
                        className="flex flex-wrap items-center justify-between gap-2 rounded-md bg-background/60 px-2 py-1.5 text-xs"
                      >
                        <span className="flex min-w-0 max-w-full items-center gap-2">
                          <span className="truncate font-medium text-foreground">
                            {c.channel_name}
                          </span>
                          <span className="shrink-0 text-muted-foreground">
                            {c.source === "missing"
                              ? "无历史金额，按 0 计入"
                              : `沿用 ${money(c.cost, { precise: true })}`}
                          </span>
                        </span>
                        <Button
                          type="button"
                          size="sm"
                          variant="outline"
                          className="h-7 gap-1 px-2 text-xs"
                          disabled={channel == null}
                          onClick={() => setEditingChannel(channel ?? null)}
                        >
                          <Settings2 className="size-3.5" />
                          {"打开设置"}
                        </Button>
                      </li>
                    )
                  })}
                </ul>
              </div>
            ) : null}
          </div>
        )}

        {configured ? (
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => onOpenChange(false)}
            >
              {"关闭"}
            </Button>
            {failedChannels.length > 0 ? (
              <Button
                type="button"
                size="sm"
                variant="outline"
                className="gap-1"
                disabled={running || !enabled}
                onClick={handleRetry}
              >
                <RefreshCw className={cn("size-3.5", retrying && "animate-spin")} />
                {`重试 ${failedChannels.length} 个供应商`}
              </Button>
            ) : null}
            <Button
              type="button"
              size="sm"
              className="gap-1"
              disabled={running || !enabled}
              onClick={() => void run("refresh")}
            >
              <RefreshCw className={cn("size-3.5", running && !retrying && "animate-spin")} />
              {running ? "更新中…" : "重新更新"}
            </Button>
          </div>
        ) : null}

        <ProfitSiteDialog
          open={siteDialogOpen}
          onOpenChange={(v) => {
            setSiteDialogOpen(v)
            if (!v) setEditingSite(null)
          }}
          site={editingSite}
          onSaved={handleSiteSaved}
        />
        <ChannelFormDialog
          open={editingChannel != null}
          onOpenChange={(v) => {
            if (!v) setEditingChannel(null)
          }}
          channel={editingChannel}
        />
      </DialogContent>
    </Dialog>
  )
}

/** 结果区：先给合计四项，再逐站点列出各自的收益。 */
function ProfitResultPanel({ summary }: { summary: ProfitSummary }) {
  const positive = summary.net_profit >= 0
  const freshCount = summary.channels.filter((c) => c.source === "fresh").length
  const failedCount = summary.channels.filter(
    (c) => c.source === "fallback" || c.source === "missing",
  ).length
  const carriedCount = summary.channels.length - freshCount - failedCount
  const sites = summary.sites ?? []
  return (
    <div className="space-y-1.5 rounded-lg border border-border bg-background px-3 py-2.5">
      <ResultRow
        operator=""
        label={`总收益（${sites.filter((s) => s.sampled).length} 个站点）`}
        value={money(summary.revenue, { precise: true })}
        warning={!summary.revenue_ok}
      />
      <ResultRow
        operator="−"
        label="管理员消费"
        value={money(summary.admin_cost, { precise: true })}
        warning={!summary.admin_ok}
      />
      <ResultRow
        operator="−"
        label="成本"
        value={money(summary.channel_cost, { precise: true })}
      />
      <div className="my-1 border-t border-border" />
      <ResultRow
        operator="="
        label="净收益"
        value={money(summary.net_profit, { precise: true })}
        strong
        tone={positive ? "text-success" : "text-danger"}
      />
      {sites.length > 0 ? (
        <>
          <div className="my-1 border-t border-border" />
          <ul className="space-y-1">
            {sites.map((s) => (
              <SiteProfitRow key={s.site_id} site={s} />
            ))}
          </ul>
        </>
      ) : null}
      <p className="pt-1 text-[11px] text-muted-foreground">
        {`${freshCount} 个供应商本轮重新采集`}
        {carriedCount > 0 ? ` · ${carriedCount} 个本轮未采集` : ""}
        {failedCount > 0 ? ` · ${failedCount} 个采集失败` : ""}
      </p>
    </div>
  )
}

/** 站点失败原因：优先用后端给的错误，其次按缺了哪一项具体说明。 */
function siteFailReason(site: ProfitSiteProfit): string {
  if (site.error) return site.error
  if (!site.revenue_ok && !site.admin_ok) return "今日总收益与管理员消费都没取到"
  if (!site.revenue_ok) return "今日总收益没取到"
  return "管理员消费没取到"
}

/** 单个站点的收益行：站点名 + 站点收益，副行给出总收益与管理员消费。 */
function SiteProfitRow({ site }: { site: ProfitSiteProfit }) {
  const failed = site.sampled && (site.error != null || !site.revenue_ok || !site.admin_ok)
  const positive = site.site_profit >= 0
  return (
    <li className="flex items-start justify-between gap-2 text-xs">
      <span className="min-w-0">
        <span className="flex items-center gap-1.5">
          <span className="truncate font-medium text-foreground">{site.site_name}</span>
          {failed ? <AlertTriangle className="size-3 shrink-0 text-warning" /> : null}
        </span>
        <span className="block truncate text-[11px] text-muted-foreground">
          {site.sampled
            ? `收入 ${money(site.revenue, { precise: true })} − 管理员 ${money(site.admin_cost, { precise: true })}`
            : "本轮未计算"}
        </span>
      </span>
      {site.sampled ? (
        <span
          className={cn(
            "shrink-0 tabular-nums font-medium",
            positive ? "text-success" : "text-danger",
          )}
        >
          {money(site.site_profit, { precise: true })}
        </span>
      ) : (
        <span className="shrink-0 text-muted-foreground">{"待更新"}</span>
      )}
    </li>
  )
}

function ResultRow({
  operator,
  label,
  value,
  strong = false,
  warning = false,
  tone,
}: {
  operator: string
  label: string
  value: string
  strong?: boolean
  warning?: boolean
  tone?: string
}) {
  return (
    <div className="flex items-center justify-between gap-2">
      <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <span className="w-3 text-center">{operator}</span>
        {label}
        {warning ? <AlertTriangle className="size-3 text-warning" /> : null}
      </span>
      <span
        className={cn(
          "tabular-nums",
          strong ? "text-base font-bold" : "font-medium text-foreground",
          tone,
        )}
      >
        {value}
      </span>
    </div>
  )
}
