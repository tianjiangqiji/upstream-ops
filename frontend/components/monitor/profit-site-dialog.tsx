"use client"

import { useCallback, useEffect, useState } from "react"
import { Loader2 } from "lucide-react"
import { toast } from "sonner"

import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
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
import type { ProfitCredentialMode, ProfitSite, ProfitSiteInput } from "@/lib/api-types"

interface Draft {
  name: string
  site_url: string
  credential_mode: ProfitCredentialMode
  user_id: string
  access_token: string
  cookie: string
  sort_order: string
  enabled: boolean
}

const EMPTY_DRAFT: Draft = {
  name: "",
  site_url: "",
  credential_mode: "token",
  user_id: "",
  access_token: "",
  cookie: "",
  sort_order: "0",
  enabled: true,
}

function draftFrom(site?: ProfitSite | null): Draft {
  if (!site) return { ...EMPTY_DRAFT }
  return {
    name: site.name,
    site_url: site.site_url,
    credential_mode: site.credential_mode || "token",
    user_id: site.user_id,
    access_token: "",
    cookie: "",
    sort_order: String(site.sort_order ?? 0),
    enabled: site.enabled,
  }
}

interface ProfitSiteDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** 传入表示编辑，为空表示新增 */
  site?: ProfitSite | null
  /** 保存成功后回调，调用方负责刷新列表 */
  onSaved: () => void
}

/**
 * 自有站点（NewAPI）配置弹窗。
 *
 * 一个站点就是一份「地址 + 管理员凭据」，收益按站点分别统计：
 * 站点总收益减去该站管理员消费，就是该站自己的收益。
 * 凭据留空表示沿用已保存的值，所以编辑时永远不回填明文。
 */
export function ProfitSiteDialog({ open, onOpenChange, site, onSaved }: ProfitSiteDialogProps) {
  const [draft, setDraft] = useState<Draft>(EMPTY_DRAFT)
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (!open) return
    setDraft(draftFrom(site))
  }, [open, site])

  const patch = useCallback((next: Partial<Draft>) => {
    setDraft((prev) => ({ ...prev, ...next }))
  }, [])

  const tokenMode = draft.credential_mode === "token"
  const hasCredential = tokenMode ? (site?.has_token ?? false) : (site?.has_cookie ?? false)
  const credentialTyped = tokenMode ? draft.access_token.trim() : draft.cookie.trim()
  const canSave = draft.site_url.trim() !== "" && (hasCredential || credentialTyped !== "")

  const handleSave = useCallback(async () => {
    setSaving(true)
    try {
      const payload: ProfitSiteInput = {
        name: draft.name.trim(),
        site_url: draft.site_url.trim(),
        credential_mode: draft.credential_mode,
        user_id: draft.user_id.trim(),
        enabled: draft.enabled,
        sort_order: Number(draft.sort_order) || 0,
      }
      if (tokenMode) {
        if (draft.access_token.trim()) payload.access_token = draft.access_token.trim()
      } else if (draft.cookie.trim()) {
        payload.cookie = draft.cookie.trim()
      }
      if (site) {
        await apiFetch<ProfitSite>(`/profit/sites/${site.id}`, {
          method: "PUT",
          body: JSON.stringify(payload),
        })
        toast.success("站点已更新")
      } else {
        await apiFetch<ProfitSite>("/profit/sites", {
          method: "POST",
          body: JSON.stringify(payload),
        })
        toast.success("站点已添加")
      }
      onSaved()
      onOpenChange(false)
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "保存站点失败")
    } finally {
      setSaving(false)
    }
  }, [draft, site, tokenMode, onSaved, onOpenChange])

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="text-base font-medium">
            {site ? "编辑自有站点" : "添加自有站点"}
          </DialogTitle>
          <DialogDescription className="text-xs">
            {"只支持 NewAPI：收益取该站日志统计，管理员消费取该站个人日志统计。"}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="profit-site-name" className="text-xs text-muted-foreground">
              {"站点名称"}
            </Label>
            <Input
              id="profit-site-name"
              value={draft.name}
              placeholder="留空自动取域名"
              onChange={(e) => patch({ name: e.target.value })}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="profit-site-url" className="text-xs text-muted-foreground">
              {"站点地址"}
            </Label>
            <Input
              id="profit-site-url"
              value={draft.site_url}
              placeholder="https://your-newapi.example.com"
              onChange={(e) => patch({ site_url: e.target.value })}
            />
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label className="text-xs text-muted-foreground">{"凭据模式"}</Label>
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
            <div className="space-y-1.5">
              <Label htmlFor="profit-site-user" className="text-xs text-muted-foreground">
                {"管理员用户 ID"}
              </Label>
              <Input
                id="profit-site-user"
                value={draft.user_id}
                placeholder="1"
                onChange={(e) => patch({ user_id: e.target.value })}
              />
            </div>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="profit-site-credential" className="text-xs text-muted-foreground">
              {tokenMode ? "Access Token" : "Cookie"}
            </Label>
            <Input
              id="profit-site-credential"
              type="password"
              value={tokenMode ? draft.access_token : draft.cookie}
              placeholder={
                hasCredential
                  ? "••••••（留空保持不变）"
                  : tokenMode
                    ? "粘贴管理员 access token"
                    : "粘贴管理员登录 Cookie"
              }
              onChange={(e) =>
                patch(tokenMode ? { access_token: e.target.value } : { cookie: e.target.value })
              }
            />
            <p className="text-[11px] text-muted-foreground">
              {"凭据加密保存，只用于读取该站日志统计（需要管理员权限）。"}
            </p>
          </div>
          <div className="grid gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="profit-site-sort" className="text-xs text-muted-foreground">
                {"排序（越大越靠前）"}
              </Label>
              <Input
                id="profit-site-sort"
                inputMode="numeric"
                value={draft.sort_order}
                onChange={(e) => patch({ sort_order: e.target.value })}
              />
            </div>
            <div className="flex items-center justify-between gap-3 self-end rounded-xl border border-border bg-background/90 px-3 py-2">
              <Label htmlFor="profit-site-enabled" className="text-xs text-muted-foreground">
                {"参与收益计算"}
              </Label>
              <Switch
                id="profit-site-enabled"
                checked={draft.enabled}
                onCheckedChange={(v) => patch({ enabled: v })}
              />
            </div>
          </div>
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" size="sm" onClick={() => onOpenChange(false)}>
            {"取消"}
          </Button>
          <Button type="button" size="sm" className="gap-1" disabled={!canSave || saving} onClick={handleSave}>
            {saving ? <Loader2 className="size-3.5 animate-spin" /> : null}
            {"保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
