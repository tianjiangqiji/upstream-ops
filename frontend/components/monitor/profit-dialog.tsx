"use client"

import { useCallback, useEffect, useMemo, useRef, useState } from "react"
import {
  AlertTriangle,
  CheckCircle2,
  Loader2,
  RefreshCw,
  Settings2,
  TrendingUp,
  XCircle,
} from "lucide-react"
import { toast } from "sonner"

import { ChannelFormDialog } from "@/components/monitor/channel-form-dialog"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Progress } from "@/components/ui/progress"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { apiFetch } from "@/lib/api"
import { money, relativeTime } from "@/lib/format"
import { useApi, useChannels, useProfitSummary } from "@/lib/queries"
import { useTriggerRefresh } from "@/lib/refresh-context"
import { refreshProfitStream, retryProfitStream } from "@/lib/sync-stream"
import { cn } from "@/lib/utils"
import type {
  Channel,
  ProfitConfig,
  ProfitConfigInput,
  ProfitCredentialMode,
  ProfitSummary,
} from "@/lib/api-types"
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
 * 步骤归并键：主站两个步骤是全局的（没有 channel_id），
 * 供应商消费步骤每个渠道一条，靠 channel_id 区分。
 */
function stepKey(ev: ProgressEvent): string {
  return `${ev.stage}#${ev.channel_id ?? 0}`
}

function isFinished(ev: ProgressEvent): boolean {
  return ev.ok === true || ev.ok === false || ev.stage === "done" || ev.stage === "error"
}

/** 渠道消费步骤用渠道名当标签，主站步骤用固定标签。 */
function stepLabel(ev: ProgressEvent): string {
  if (ev.channel_id != null && ev.stage === "cost") {
    return ev.channel_name?.trim() || `#${ev.channel_id}`
  }
  return stageLabel[ev.stage] ?? "进度"
}

interface ProfitConfigDraft {
  site_url: string
  credential_mode: ProfitCredentialMode
  user_id: string
  access_token: string
  cookie: string
}

const EMPTY_CONFIG_DRAFT: ProfitConfigDraft = {
  site_url: "",
  credential_mode: "token",
  user_id: "",
  access_token: "",
  cookie: "",
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
 * 一轮更新的完整过程都在这里可见：先拉主站总收益，再拉管理员测试消费，
 * 然后逐个供应商重新采集今日消费并累减。采集失败的供应商不会让流程中断，
 * 而是沿用上一次成功的金额计入，同时列在下方供用户重试，
 * 或直接打开该渠道的设置修正凭据后再重试。
 */
export function ProfitDialog({ open, onOpenChange, autoStart = true }: ProfitDialogProps) {
  const summaryQuery = useProfitSummary()
  const configQuery = useApi<ProfitConfig>("/profit/config")
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
  const [configDraft, setConfigDraft] = useState<ProfitConfigDraft>(EMPTY_CONFIG_DRAFT)
  const [savingConfig, setSavingConfig] = useState(false)

  const streamRef = useRef<AbortController | null>(null)
  // 每次打开只自动跑一轮，避免轮询把 SSE 反复触发。
  const autoStartedRef = useRef(false)

  const configured = summary?.configured ?? false
  const enabled = summary?.enabled ?? true
  const hasSnapshot = summary?.has_snapshot ?? false

  const applyEvent = useCallback((ev: ProgressEvent) => {
    if (ev.total != null) setChannelTotal(ev.total)
    const payload = ev.data as { summary?: ProfitSummary } | undefined
    if (payload?.summary) setSummary(payload.summary)
    // 没有 channel_id 的 error 是流程级结论（含"计算完成但有渠道失败"），
    // 只在顶部提示条展示，不进步骤列表，避免同一条消息出现两次。
    if (ev.stage === "error" && ev.channel_id == null) {
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

  // 未配置主站时把已保存的地址/用户 ID 带进表单，凭据永远留空。
  useEffect(() => {
    const cfg = configQuery.data
    if (!open || cfg == null || cfg.configured) return
    setConfigDraft((prev) => {
      if (prev.site_url !== "" || prev.user_id !== "") return prev
      return {
        ...prev,
        site_url: cfg.site_url,
        credential_mode: cfg.credential_mode || "token",
        user_id: cfg.user_id,
      }
    })
  }, [open, configQuery.data])

  // 配置好后打开弹窗即自动更新一轮。
  useEffect(() => {
    if (!open || !autoStart || autoStartedRef.current) return
    const data = summaryQuery.data
    if (data == null || !data.configured || !data.enabled) return
    autoStartedRef.current = true
    void run("refresh")
  }, [open, autoStart, summaryQuery.data, run])

  useEffect(() => () => abort(), [abort])

  const failedChannels = useMemo(
    () => (summary?.channels ?? []).filter((c) => c.source !== "fresh"),
    [summary],
  )

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
  const expectedSteps = Math.max(3 + channelTotal, finishedCount, 1)
  const percent = Math.min(100, Math.round((finishedCount / expectedSteps) * 100))

  const handleSaveConfig = useCallback(async () => {
    setSavingConfig(true)
    try {
      const payload: ProfitConfigInput = {
        site_url: configDraft.site_url.trim(),
        credential_mode: configDraft.credential_mode,
        user_id: configDraft.user_id.trim(),
        enabled: true,
      }
      if (configDraft.credential_mode === "token") {
        if (configDraft.access_token.trim()) payload.access_token = configDraft.access_token.trim()
      } else if (configDraft.cookie.trim()) {
        payload.cookie = configDraft.cookie.trim()
      }
      const view = await apiFetch<ProfitConfig>("/profit/config", {
        method: "PUT",
        body: JSON.stringify(payload),
      })
      setConfigDraft(EMPTY_CONFIG_DRAFT)
      setSummary((prev) => ({
        ...(prev ?? EMPTY_SUMMARY),
        configured: view.configured,
        enabled: view.enabled,
        site_url: view.site_url,
      }))
      void configQuery.refetch()
      toast.success("主站配置已保存")
      void run("refresh")
    } catch (e) {
      toast.error((e as Error).message || "保存主站配置失败")
    } finally {
      setSavingConfig(false)
    }
  }, [configDraft, run, configQuery])

  const handleRetry = useCallback(() => {
    const ids = failedChannels.map((c) => c.channel_id)
    if (ids.length === 0) return
    void run("retry", ids)
  }, [failedChannels, run])

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
                : "正在获取主站收益并逐个采集供应商消费…"
              : hasSnapshot
                ? `上次更新于 ${relativeTime(summary?.sampled_at)}，收益不是实时数据`
                : "尚未更新过，点击下方按钮开始"}
          </DialogDescription>
        </DialogHeader>

        {!configured ? (
          <ProfitConfigForm
            draft={configDraft}
            onChange={setConfigDraft}
            saving={savingConfig}
            onSave={handleSaveConfig}
          />
        ) : (
          <div className="space-y-3">
            <div className="space-y-1.5">
              <div className="flex items-center justify-between text-xs text-muted-foreground">
                <span>
                  {running
                    ? `已完成 ${finishedCount} / ${expectedSteps} 步`
                    : fatal
                      ? finishedCount > 0
                        ? "已完成，部分供应商采集失败"
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
                        <span className="w-12 shrink-0 text-[11px] text-muted-foreground">
                          {step.label}
                        </span>
                        <span
                          className={cn(
                            "block min-w-0 flex-1 whitespace-pre-wrap",
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
                <span className="whitespace-pre-wrap">{fatal}</span>
              </div>
            ) : null}

            {hasSnapshot && summary ? <ProfitResultPanel summary={summary} /> : null}

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
                        <span className="flex min-w-0 items-center gap-2">
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

function ProfitConfigForm({
  draft,
  onChange,
  saving,
  onSave,
}: {
  draft: ProfitConfigDraft
  onChange: (next: ProfitConfigDraft) => void
  saving: boolean
  onSave: () => void
}) {
  const tokenMode = draft.credential_mode === "token"
  const canSave =
    draft.site_url.trim() !== "" && (tokenMode ? draft.access_token.trim() !== "" : draft.cookie.trim() !== "")

  return (
    <div className="space-y-3">
      <div className="flex items-start gap-2 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning">
        <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
        <span>
          {"收益统计需要先配置你自己的 NewAPI 主站：总收益取自主站日志统计，管理员消费取自主站个人日志统计。"}
        </span>
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="profit-site-url" className="text-xs text-muted-foreground">
          {"主站地址"}
        </Label>
        <Input
          id="profit-site-url"
          value={draft.site_url}
          placeholder="https://your-newapi.example.com"
          onChange={(e) => onChange({ ...draft, site_url: e.target.value })}
        />
      </div>
      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-1.5">
          <Label className="text-xs text-muted-foreground">{"凭据模式"}</Label>
          <Select
            value={draft.credential_mode}
            onValueChange={(value) =>
              onChange({ ...draft, credential_mode: value as ProfitCredentialMode })
            }
          >
            <SelectTrigger className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="token">{"Access Token"}</SelectItem>
              <SelectItem value="cookie">{"Cookie"}</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="profit-user-id" className="text-xs text-muted-foreground">
            {"管理员用户 ID"}
          </Label>
          <Input
            id="profit-user-id"
            value={draft.user_id}
            placeholder="1"
            onChange={(e) => onChange({ ...draft, user_id: e.target.value })}
          />
        </div>
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="profit-credential" className="text-xs text-muted-foreground">
          {tokenMode ? "Access Token" : "Cookie"}
        </Label>
        <Input
          id="profit-credential"
          type="password"
          value={tokenMode ? draft.access_token : draft.cookie}
          placeholder={tokenMode ? "粘贴管理员 access token" : "粘贴管理员登录 Cookie"}
          onChange={(e) =>
            onChange(
              tokenMode
                ? { ...draft, access_token: e.target.value }
                : { ...draft, cookie: e.target.value },
            )
          }
        />
        <p className="text-[11px] text-muted-foreground">
          {"凭据加密保存，只用于读取主站日志统计（需要管理员权限）。"}
        </p>
      </div>
      <div className="flex justify-end">
        <Button
          type="button"
          size="sm"
          className="gap-1"
          disabled={!canSave || saving}
          onClick={onSave}
        >
          {saving ? <Loader2 className="size-3.5 animate-spin" /> : null}
          {"保存并开始"}
        </Button>
      </div>
    </div>
  )
}

/** 结果区只展示四项：总收益、管理员消费、成本、净收益。 */
function ProfitResultPanel({ summary }: { summary: ProfitSummary }) {
  const positive = summary.net_profit >= 0
  const freshCount = summary.channels.filter((c) => c.source === "fresh").length
  const carriedCount = summary.channels.length - freshCount
  return (
    <div className="space-y-1.5 rounded-lg border border-border bg-background px-3 py-2.5">
      <ResultRow
        operator=""
        label="总收益"
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
      <p className="pt-1 text-[11px] text-muted-foreground">
        {`${freshCount} 个供应商本轮重新采集`}
        {carriedCount > 0 ? ` · ${carriedCount} 个沿用上次金额` : ""}
      </p>
    </div>
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

const EMPTY_SUMMARY: ProfitSummary = {
  configured: false,
  enabled: false,
  site_url: "",
  day: "",
  has_snapshot: false,
  revenue: 0,
  admin_cost: 0,
  channel_cost: 0,
  net_profit: 0,
  revenue_ok: false,
  admin_ok: false,
  channel_ok: 0,
  channel_failed: 0,
  channels: [],
  running: false,
  snapshot_failed: 0,
}
