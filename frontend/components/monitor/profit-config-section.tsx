"use client"

import { useCallback, useEffect, useState } from "react"
import { Loader2, PlugZap, Save, TrendingUp } from "lucide-react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import { apiFetch } from "@/lib/api"
import { money } from "@/lib/format"
import { useApi } from "@/lib/queries"
import type {
  ProfitConfig,
  ProfitConfigInput,
  ProfitConfigTestResult,
  ProfitCredentialMode,
} from "@/lib/api-types"

interface Draft {
  site_url: string
  credential_mode: ProfitCredentialMode
  user_id: string
  access_token: string
  cookie: string
  enabled: boolean
}

const EMPTY_DRAFT: Draft = {
  site_url: "",
  credential_mode: "token",
  user_id: "",
  access_token: "",
  cookie: "",
  enabled: true,
}

function draftFrom(config: ProfitConfig): Draft {
  return {
    site_url: config.site_url,
    credential_mode: config.credential_mode || "token",
    user_id: config.user_id,
    access_token: "",
    cookie: "",
    enabled: config.enabled,
  }
}

/**
 * 「收益统计」配置区块。
 *
 * 收益不是渠道数据，而是管理员自有主站（NewAPI）的日志统计，
 * 所以这里单独维护主站地址与管理员凭据，保存后立即写库生效（不进配置文件）。
 */
export function ProfitConfigSection() {
  const query = useApi<ProfitConfig>("/profit/config")
  const [draft, setDraft] = useState<Draft>(EMPTY_DRAFT)
  const [loaded, setLoaded] = useState(false)
  const [saving, setSaving] = useState(false)
  const [testing, setTesting] = useState(false)

  useEffect(() => {
    if (query.data && !loaded) {
      setDraft(draftFrom(query.data))
      setLoaded(true)
    }
  }, [query.data, loaded])

  const patch = useCallback((next: Partial<Draft>) => {
    setDraft((prev) => ({ ...prev, ...next }))
  }, [])

  const tokenMode = draft.credential_mode === "token"
  const hasCredential = tokenMode ? query.data?.has_token : query.data?.has_cookie
  const credentialTyped = tokenMode ? draft.access_token.trim() : draft.cookie.trim()
  const canSave = draft.site_url.trim() !== "" && (hasCredential || credentialTyped !== "")

  const handleSave = useCallback(async () => {
    setSaving(true)
    try {
      const payload: ProfitConfigInput = {
        site_url: draft.site_url.trim(),
        credential_mode: draft.credential_mode,
        user_id: draft.user_id.trim(),
        enabled: draft.enabled,
      }
      if (tokenMode) {
        if (draft.access_token.trim()) payload.access_token = draft.access_token.trim()
      } else if (draft.cookie.trim()) {
        payload.cookie = draft.cookie.trim()
      }
      const view = await apiFetch<ProfitConfig>("/profit/config", {
        method: "PUT",
        body: JSON.stringify(payload),
      })
      query.setData(view)
      setDraft(draftFrom(view))
      toast.success("收益统计配置已保存")
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "保存收益统计配置失败")
    } finally {
      setSaving(false)
    }
  }, [draft, query, tokenMode])

  const handleTest = useCallback(async () => {
    setTesting(true)
    try {
      const result = await apiFetch<ProfitConfigTestResult>("/profit/config/test", {
        method: "POST",
      })
      toast.success(`${result.message}：近 24 小时 ${money(result.revenue, { precise: true })}`)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "测试连接失败")
    } finally {
      setTesting(false)
    }
  }, [])

  return (
    <section className="rounded-3xl border border-border/80 bg-muted/20 p-5">
      <div className="mb-5 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="space-y-1.5">
          <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
            <TrendingUp className="size-4 text-emerald-600" />
            {"收益统计"}
          </div>
          <p className="max-w-2xl text-sm leading-6 text-muted-foreground">
            {"今日收益由三部分组成：主站日志统计的今日总额减去管理员自己的测试消费，再减去所有供应商的今日消费。主站需要管理员权限，凭据加密保存在数据库，保存后立即生效。"}
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            className="gap-1"
            disabled={testing || !query.data?.configured}
            onClick={handleTest}
          >
            {testing ? (
              <Loader2 className="size-3.5 animate-spin" />
            ) : (
              <PlugZap className="size-3.5" />
            )}
            {"测试连接"}
          </Button>
          <Button size="sm" className="gap-1" disabled={saving || !canSave} onClick={handleSave}>
            {saving ? <Loader2 className="size-3.5 animate-spin" /> : <Save className="size-3.5" />}
            {"保存"}
          </Button>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <div className="space-y-2">
          <div className="space-y-1">
            <Label className="text-xs font-medium text-foreground">{"主站地址"}</Label>
            <p className="text-[11px] leading-5 text-muted-foreground">
              {"你自己的 NewAPI 站点根地址，例如 https://api.example.com。"}
            </p>
          </div>
          <Input
            value={draft.site_url}
            placeholder="https://your-newapi.example.com"
            onChange={(e) => patch({ site_url: e.target.value })}
          />
        </div>
        <div className="space-y-2">
          <div className="space-y-1">
            <Label className="text-xs font-medium text-foreground">{"管理员用户 ID"}</Label>
            <p className="text-[11px] leading-5 text-muted-foreground">
              {"主站里管理员账号的数字 ID，用于 New-Api-User 请求头。"}
            </p>
          </div>
          <Input
            value={draft.user_id}
            placeholder="1"
            onChange={(e) => patch({ user_id: e.target.value })}
          />
        </div>
        <div className="space-y-2">
          <div className="space-y-1">
            <Label className="text-xs font-medium text-foreground">{"凭据模式"}</Label>
            <p className="text-[11px] leading-5 text-muted-foreground">
              {"token 直连更稳定；cookie 适合只能复用登录态的主站。"}
            </p>
          </div>
          <Select
            value={draft.credential_mode}
            onValueChange={(v) => patch({ credential_mode: v as ProfitCredentialMode })}
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
        <div className="space-y-2">
          <div className="space-y-1">
            <Label className="text-xs font-medium text-foreground">
              {tokenMode ? "Access Token" : "Cookie"}
            </Label>
            <p className="text-[11px] leading-5 text-muted-foreground">
              {hasCredential ? "已保存凭据，留空表示保持不变。" : "尚未保存凭据，需要填写后才能更新收益。"}
            </p>
          </div>
          <Input
            type="password"
            value={tokenMode ? draft.access_token : draft.cookie}
            placeholder={hasCredential ? "••••••（留空保持不变）" : tokenMode ? "粘贴管理员 access token" : "粘贴管理员登录 Cookie"}
            onChange={(e) =>
              patch(
                tokenMode ? { access_token: e.target.value } : { cookie: e.target.value },
              )
            }
          />
        </div>
      </div>

      <div className="mt-4 flex items-start justify-between gap-4 rounded-2xl border border-border bg-background/90 px-4 py-3">
        <div className="space-y-1">
          <Label htmlFor="profit-enabled" className="text-sm font-medium text-foreground">
            {"启用收益统计"}
          </Label>
          <p className="text-[11px] leading-5 text-muted-foreground">
            {"关闭后顶部「今日收益」卡片仍显示历史结果，但不再允许发起更新。"}
          </p>
        </div>
        <Switch
          id="profit-enabled"
          checked={draft.enabled}
          onCheckedChange={(v) => patch({ enabled: v })}
        />
      </div>
    </section>
  )
}
