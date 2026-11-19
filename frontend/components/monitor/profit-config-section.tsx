"use client"

import { useCallback, useMemo, useState } from "react"
import {
  AlertTriangle,
  CheckCircle2,
  Loader2,
  Pencil,
  PlugZap,
  Plus,
  Trash2,
  TrendingUp,
} from "lucide-react"
import { toast } from "sonner"

import { ProfitSiteDialog } from "@/components/monitor/profit-site-dialog"
import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { useConfirm } from "@/components/ui/confirm-dialog"
import { apiFetch } from "@/lib/api"
import { money } from "@/lib/format"
import { useProfitConfig, useProfitSummary } from "@/lib/queries"
import { cn } from "@/lib/utils"
import type {
  ProfitConfig,
  ProfitSite,
  ProfitSiteProfit,
  ProfitSiteTestResult,
} from "@/lib/api-types"

/**
 * 「收益统计」配置区块。
 *
 * 收益不是渠道数据，而是若干管理员自有站点（NewAPI）的日志统计：
 * 每个站点各自算「站点总收益 − 该站管理员消费」，再加总后减去供应商成本。
 * 所以这里维护的是一个站点列表，凭据加密落库，保存后立即生效（不进配置文件）。
 */
export function ProfitConfigSection() {
  const query = useProfitConfig()
  const summary = useProfitSummary()
  const { confirm, dialog } = useConfirm()

  const [editing, setEditing] = useState<ProfitSite | null>(null)
  const [creating, setCreating] = useState(false)
  const [testingID, setTestingID] = useState<number | null>(null)
  const [deletingID, setDeletingID] = useState<number | null>(null)
  const [togglingEnabled, setTogglingEnabled] = useState(false)

  const config: ProfitConfig | null = query.data
  const sites = config?.sites ?? []
  const enabled = config?.enabled ?? false

  // 最近一次更新的逐站点收益，用来在站点行上直接显示各自的收益。
  const profits = useMemo(() => {
    const map = new Map<number, ProfitSiteProfit>()
    for (const p of summary.data?.sites ?? []) map.set(p.site_id, p)
    return map
  }, [summary.data])
  const hasSnapshot = summary.data?.has_snapshot ?? false

  const handleSaved = useCallback(() => {
    query.refetch()
  }, [query])

  const handleToggleEnabled = useCallback(
    async (next: boolean) => {
      setTogglingEnabled(true)
      try {
        const view = await apiFetch<ProfitConfig>("/profit/config", {
          method: "PUT",
          body: JSON.stringify({ enabled: next }),
        })
        query.setData(view)
        toast.success(next ? "已启用收益统计" : "已关闭收益统计")
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "保存总开关失败")
      } finally {
        setTogglingEnabled(false)
      }
    },
    [query],
  )

  const handleToggleSite = useCallback(
    async (site: ProfitSite, next: boolean) => {
      try {
        await apiFetch<ProfitSite>(`/profit/sites/${site.id}`, {
          method: "PUT",
          body: JSON.stringify({
            name: site.name,
            site_url: site.site_url,
            credential_mode: site.credential_mode,
            user_id: site.user_id,
            enabled: next,
            sort_order: site.sort_order,
          }),
        })
        query.refetch()
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "切换站点状态失败")
      }
    },
    [query],
  )

  const handleTest = useCallback(async (site: ProfitSite) => {
    setTestingID(site.id)
    try {
      const result = await apiFetch<ProfitSiteTestResult>(`/profit/sites/${site.id}/test`, {
        method: "POST",
      })
      toast.success(`${result.site_name}：近 24 小时 ${money(result.revenue, { precise: true })}`)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "测试连接失败")
    } finally {
      setTestingID(null)
    }
  }, [])

  const handleDelete = useCallback(
    async (site: ProfitSite) => {
      const ok = await confirm({
        title: `删除站点「${site.name}」？`,
        description: "删除后该站不再参与收益计算，历史快照里的金额不受影响。",
        confirmLabel: "删除",
        destructive: true,
      })
      if (!ok) return
      setDeletingID(site.id)
      try {
        await apiFetch(`/profit/sites/${site.id}`, { method: "DELETE" })
        toast.success("站点已删除")
        query.refetch()
      } catch (err) {
        toast.error(err instanceof Error ? err.message : "删除站点失败")
      } finally {
        setDeletingID(null)
      }
    },
    [confirm, query],
  )

  return (
    <section className="rounded-3xl border border-border/80 bg-muted/20 p-5">
      <div className="mb-5 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="space-y-1.5">
          <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
            <TrendingUp className="size-4 text-emerald-600" />
            {"收益统计"}
          </div>
          <p className="max-w-2xl text-sm leading-6 text-muted-foreground">
            {"今日收益 = 各站点日志统计的今日总额 − 各站点管理员测试消费 − 所有供应商的今日消费。每个站点分别统计并展示，可添加多个。站点需要管理员权限，凭据加密保存在数据库，保存后立即生效。"}
          </p>
        </div>
        <Button
          size="sm"
          className="shrink-0 gap-1"
          onClick={() => setCreating(true)}
        >
          <Plus className="size-3.5" />
          {"添加站点"}
        </Button>
      </div>

      {sites.length === 0 ? (
        <div className="rounded-2xl border border-dashed border-border bg-background/60 px-4 py-6 text-center text-sm text-muted-foreground">
          {"还没有自有站点。点击右上角「添加站点」，填入你自己的 NewAPI 地址与管理员凭据。"}
        </div>
      ) : (
        <ul className="space-y-2.5">
          {sites.map((site) => {
            const ready = site.configured && site.enabled
            const profit = profits.get(site.id)
            // 快照里还没有这个站点的明细时（刚添加或刚升级），不算失败，显示待更新。
            const profitSampled = profit?.sampled ?? false
            const profitFailed =
              profitSampled &&
              profit != null &&
              (profit.error != null || !profit.revenue_ok || !profit.admin_ok)
            return (
              <li
                key={site.id}
                className="rounded-2xl border border-border bg-background/90 px-4 py-3"
              >
                <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
                  <div className="min-w-0 space-y-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="truncate text-sm font-medium text-foreground">
                        {site.name}
                      </span>
                      <span
                        className={cn(
                          "shrink-0 rounded-full px-2 py-0.5 text-[11px]",
                          ready
                            ? "bg-success/10 text-success"
                            : site.enabled
                              ? "bg-warning/10 text-warning"
                              : "bg-muted text-muted-foreground",
                        )}
                      >
                        {site.enabled
                          ? site.configured
                            ? "参与计算"
                            : "待补凭据"
                          : "已停用"}
                      </span>
                    </div>
                    <p className="truncate text-xs text-muted-foreground">{site.site_url}</p>
                    <p className="text-[11px] text-muted-foreground">
                      {site.credential_mode === "cookie" ? "Cookie" : "Access Token"}
                      {site.user_id ? ` · 管理员 ID ${site.user_id}` : ""}
                      {site.sort_order !== 0 ? ` · 排序 ${site.sort_order}` : ""}
                    </p>
                    {hasSnapshot && profit && !profitSampled ? (
                      <p className="flex flex-wrap items-center gap-1.5 text-[11px] text-muted-foreground">
                        {"待更新：再更新一次即可计入"}
                      </p>
                    ) : null}
                    {hasSnapshot && profit && profitSampled ? (
                      <p className="flex flex-wrap items-center gap-1.5 text-[11px]">
                        <span
                          className={cn(
                            "font-medium tabular-nums",
                            profit.site_profit >= 0 ? "text-success" : "text-danger",
                          )}
                        >
                          {`今日收益 ${money(profit.site_profit, { precise: true })}`}
                        </span>
                        <span className="text-muted-foreground">
                          {`（收入 ${money(profit.revenue, { precise: true })} − 管理员 ${money(profit.admin_cost, { precise: true })}）`}
                        </span>
                        {profitFailed ? (
                          <AlertTriangle className="size-3 shrink-0 text-warning" />
                        ) : null}
                      </p>
                    ) : null}
                  </div>
                  <div className="flex shrink-0 items-center gap-1.5">
                    <Button
                      variant="outline"
                      size="sm"
                      className="h-7 gap-1 px-2 text-xs"
                      disabled={!site.configured || testingID === site.id}
                      onClick={() => void handleTest(site)}
                    >
                      {testingID === site.id ? (
                        <Loader2 className="size-3.5 animate-spin" />
                      ) : (
                        <PlugZap className="size-3.5" />
                      )}
                      {"测试"}
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      className="h-7 gap-1 px-2 text-xs"
                      onClick={() => setEditing(site)}
                    >
                      <Pencil className="size-3.5" />
                      {"编辑"}
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      className="h-7 gap-1 px-2 text-xs text-danger"
                      disabled={deletingID === site.id}
                      onClick={() => void handleDelete(site)}
                    >
                      {deletingID === site.id ? (
                        <Loader2 className="size-3.5 animate-spin" />
                      ) : (
                        <Trash2 className="size-3.5" />
                      )}
                      {"删除"}
                    </Button>
                    <Switch
                      checked={site.enabled}
                      aria-label={`启用站点 ${site.name}`}
                      onCheckedChange={(v) => void handleToggleSite(site, v)}
                    />
                  </div>
                </div>
              </li>
            )
          })}
        </ul>
      )}

      <div className="mt-4 flex items-start justify-between gap-4 rounded-2xl border border-border bg-background/90 px-4 py-3">
        <div className="space-y-1">
          <Label htmlFor="profit-enabled" className="text-sm font-medium text-foreground">
            {"启用收益统计"}
          </Label>
          <p className="text-[11px] leading-5 text-muted-foreground">
            {"关闭后顶部「今日收益」卡片仍显示历史结果，但不再允许发起更新。"}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {togglingEnabled ? (
            <Loader2 className="size-3.5 animate-spin text-muted-foreground" />
          ) : enabled && config?.configured ? (
            <CheckCircle2 className="size-4 text-success" />
          ) : null}
          <Switch
            id="profit-enabled"
            checked={enabled}
            disabled={togglingEnabled}
            onCheckedChange={(v) => void handleToggleEnabled(v)}
          />
        </div>
      </div>

      <ProfitSiteDialog
        open={creating}
        onOpenChange={setCreating}
        site={null}
        onSaved={handleSaved}
      />
      <ProfitSiteDialog
        open={editing != null}
        onOpenChange={(v) => {
          if (!v) setEditing(null)
        }}
        site={editing}
        onSaved={handleSaved}
      />
      {dialog}
    </section>
  )
}
