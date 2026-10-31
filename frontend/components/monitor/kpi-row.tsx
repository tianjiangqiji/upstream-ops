"use client"

import { useState } from "react"
import {
  ArrowUpRight,
  DollarSign,
  Layers,
  MessageSquare,
  TrendingUp,
  Wallet,
} from "lucide-react"
import { Card } from "@/components/ui/card"
import { ProfitDialog } from "@/components/monitor/profit-dialog"
import { cn } from "@/lib/utils"
import {
  useDashboardSummary,
  useGatewayUsageStatsToday,
  useProfitSummary,
  useRateChanges,
} from "@/lib/queries"
import { formatTokens, money, relativeTime } from "@/lib/format"
import type { LucideIcon } from "lucide-react"
import type { ReactNode } from "react"

interface Kpi {
  label: string
  value: ReactNode
  icon: LucideIcon
  iconBg: string
  iconColor: string
  footer: ReactNode
  /** 整卡可点击（例如「今日收益」点了就打开更新弹窗）。 */
  onClick?: () => void
}

function countTodayChanges(changes: { changed_at: string }[]) {
  const startOfDay = new Date()
  startOfDay.setHours(0, 0, 0, 0)
  return changes.filter((c) => new Date(c.changed_at) >= startOfDay).length
}

export function KpiRow() {
  const summary = useDashboardSummary()
  const recentChanges = useRateChanges(1, 100)
  const gatewayToday = useGatewayUsageStatsToday()
  const profit = useProfitSummary()
  const [profitOpen, setProfitOpen] = useState(false)

  const data = summary.data
  const total = data?.total_channels ?? 0
  const active = data?.active_channels ?? 0
  const failed = data?.failed_channels ?? 0
  const totalBalance = data?.total_balance ?? 0
  const todayTotalCost = data?.today_total_cost ?? 0
  const totalCost = data?.total_cost ?? 0
  const lowest = data?.lowest_balance ?? null

  const todayChangeCount = countTodayChanges(recentChanges.data?.items ?? [])

  const todayTokens = gatewayToday.data?.total_tokens ?? 0
  const todayInput = gatewayToday.data?.total_input_tokens ?? 0
  const todayOutput = gatewayToday.data?.total_output_tokens ?? 0

  // 收益要点了「更新」才算，卡片只展示最近一次结果；没有快照就显示 —。
  const profitData = profit.data
  const profitReady = profitData?.has_snapshot ?? false
  const profitValue = profitReady ? money(profitData?.net_profit, { precise: true }) : "—"
  const profitPositive = (profitData?.net_profit ?? 0) >= 0
  let profitFooter = "点击更新今日收益"
  if (profitData?.running) {
    profitFooter = "正在更新…"
  } else if (!profitData?.configured) {
    profitFooter = "点击配置主站并更新"
  } else if (profitReady) {
    profitFooter = `更新于 ${relativeTime(profitData?.sampled_at)}`
  }

  const kpis: Kpi[] = [
    {
      label: "今日收益",
      value: (
        <span className={cn(profitReady && (profitPositive ? "text-success" : "text-danger"))}>
          {profitValue}
        </span>
      ),
      icon: TrendingUp,
      iconBg: "bg-success/10",
      iconColor: "text-success",
      footer: <span className="text-muted-foreground">{profitFooter}</span>,
      onClick: () => setProfitOpen(true),
    },
    {
      label: "总余额",
      value: money(totalBalance),
      icon: DollarSign,
      iconBg: "bg-brand/10",
      iconColor: "text-brand",
      footer: lowest ? (
        <span className="text-muted-foreground">
          {"最低："}
          <span className="font-medium text-foreground">{lowest.name}</span>
          {" "}
          <span className="text-warning">{money(lowest.balance)}</span>
        </span>
      ) : (
        <span className="text-muted-foreground">{"—"}</span>
      ),
    },
    {
      label: "今日总消费",
      value: money(todayTotalCost),
      icon: Wallet,
      iconBg: "bg-warning/10",
      iconColor: "text-warning",
      footer: (
        <span className="text-muted-foreground">
          {todayTotalCost > 0 ? "按实际扣费统计" : "今日暂无消费"}
        </span>
      ),
    },
    {
      label: "累计消费",
      value: money(totalCost),
      icon: DollarSign,
      iconBg: "bg-brand/10",
      iconColor: "text-brand",
      footer: (
        <span className="text-muted-foreground">
          {totalCost > 0 ? "全渠道累计实际扣费" : "暂无累计消费"}
        </span>
      ),
    },
    {
      label: "今日 Token",
      value: formatTokens(todayTokens),
      icon: Layers,
      iconBg: "bg-brand/10",
      iconColor: "text-brand",
      footer: (
        <span className="text-muted-foreground">
          {todayTokens > 0
            ? `输入: ${formatTokens(todayInput)} / 输出: ${formatTokens(todayOutput)}`
            : "网关今日暂无调用"}
        </span>
      ),
    },
    {
      label: "渠道状态",
      value: (
        <span>
          {active}
          <span className="mx-1 text-lg font-normal text-muted-foreground">{"/"}</span>
          <span className="text-lg font-normal text-muted-foreground">{total}</span>
        </span>
      ),
      icon: MessageSquare,
      iconBg: "bg-success/10",
      iconColor: "text-success",
      footer: (
        <span className="text-muted-foreground">
          <span className="text-success font-medium">{active} 健康</span>
          {failed > 0 ? (
            <>
              {" · "}
              <span className="text-danger font-medium">{failed} 失败</span>
            </>
          ) : null}
        </span>
      ),
    },
    {
      label: "今日倍率变动",
      value: (
        <span className={cn(todayChangeCount > 0 ? "text-danger" : "text-foreground")}>
          {todayChangeCount}
        </span>
      ),
      icon: ArrowUpRight,
      iconBg: "bg-danger/10",
      iconColor: "text-danger",
      footer: (
        <span className="text-muted-foreground">
          {todayChangeCount > 0 ? `检测到 ${todayChangeCount} 次变动` : "今日无变动"}
        </span>
      ),
    },
  ]

  return (
    <>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-7">
        {kpis.map((k) => {
          const clickable = k.onClick != null
          return (
            <Card
              key={k.label}
              className={cn(
                "flex flex-row items-start justify-between gap-2 border border-border p-3 shadow-none sm:p-4",
                clickable &&
                  "cursor-pointer transition-colors hover:border-foreground/20 hover:bg-muted/40 focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:outline-none",
              )}
              role={clickable ? "button" : undefined}
              tabIndex={clickable ? 0 : undefined}
              aria-label={clickable ? `${k.label}：打开更新弹窗` : undefined}
              onClick={k.onClick}
              onKeyDown={
                clickable
                  ? (event) => {
                      if (event.key === "Enter" || event.key === " ") {
                        event.preventDefault()
                        k.onClick?.()
                      }
                    }
                  : undefined
              }
            >
              <div className="flex min-w-0 flex-col">
                <span className="text-xs text-muted-foreground">{k.label}</span>
                <p className="mt-1 text-xl font-bold tracking-tight text-foreground sm:text-2xl">
                  {k.value}
                </p>
                <p
                  className="mt-1 min-w-0 truncate text-xs"
                  title={typeof k.footer === "string" ? k.footer : undefined}
                >
                  {k.footer}
                </p>
              </div>
              <span
                className={cn(
                  "flex size-9 shrink-0 items-center justify-center rounded-xl sm:size-10",
                  k.iconBg,
                )}
              >
                <k.icon className={cn("size-5", k.iconColor)} />
              </span>
            </Card>
          )
        })}
      </div>
      <ProfitDialog open={profitOpen} onOpenChange={setProfitOpen} />
    </>
  )
}
