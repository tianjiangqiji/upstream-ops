"use client"

import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent, type ReactNode } from "react"
import { HelpCircle, X } from "lucide-react"
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
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Switch } from "@/components/ui/switch"
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover"
import type {
  Channel,
  ChannelType,
  ConnectorTokenField,
  CredentialMode,
  RechargeMultiplierMode,
} from "@/lib/api-types"
import { apiFetch } from "@/lib/api"
import { channelTagKey } from "@/lib/channel-tags"
import { useTriggerRefresh } from "@/lib/refresh-context"
import { useCaptchaConfigs, useConnectorPlugins } from "@/lib/queries"
import { cn } from "@/lib/utils"

interface ChannelFormDialogProps {
  open: boolean
  onOpenChange: (v: boolean) => void
  /** 编辑模式时传入；为空表示新增 */
  channel?: Channel | null
}

/**
 * FormState 是表单的所有可编辑字段。
 *
 * password 字段在 token 模式下不展示；
 * token 字段由当前插件声明的 token_fields 动态渲染，值单独存在 tokenValues 里。
 */
interface FormState {
  name: string
  type: ChannelType
  site_url: string
  username: string
  sort_order: string
  password: string
  login_extra_params: string

  credential_mode: CredentialMode

  balance_threshold: string
  recharge_multiplier: string
  recharge_multiplier_mode: RechargeMultiplierMode
  use_usage_stats_for_costs: boolean
  monitor_enabled: boolean
  only_created_key_groups_enabled: boolean
  skip_rates_on_auto_refresh: boolean
  turnstile_enabled: boolean
  ignore_announcements: boolean
  subscription_enabled: boolean
  proxy_enabled: boolean
  captcha_config_id: string // "" 表示不绑定

  notes: string
  // 标签：tags 为已确认的 chip，tag_input 为输入框里尚未确认的文本（提交时一并加入）
  tags: string[]
  tag_input: string
}

// 与后端 storage.MaxChannelTags / MaxChannelTagRunes / MaxChannelNotesRunes 保持一致
const MAX_TAGS = 20
const MAX_TAG_LENGTH = 32
const MAX_NOTES_LENGTH = 2000

/** charLength 按 Unicode 码点计数，与后端 utf8.RuneCountInString 对齐。 */
function charLength(s: string): number {
  return Array.from(s).length
}

/**
 * normalizeTags 与后端 storage.NormalizeChannelTags 规则一致：
 * 按半角 / 全角逗号拆分、去首尾空白、丢弃空项、只忽略 ASCII 大小写去重（保留首次出现的写法与顺序）。
 */
function normalizeTags(list: string[]): string[] {
  const out: string[] = []
  const seen = new Set<string>()
  for (const raw of list) {
    for (const part of raw.split(/[,，]/)) {
      const tag = part.trim()
      if (!tag) continue
      const key = channelTagKey(tag)
      if (seen.has(key)) continue
      seen.add(key)
      out.push(tag)
    }
  }
  return out
}

function initialState(c?: Channel | null): FormState {
  const rechargeMultiplierMode = c?.recharge_multiplier_mode === "multiply" ? "multiply" : "divide"
  return {
    name: c?.name ?? "",
    type: c?.type ?? "sub2api",
    site_url: c?.site_url ?? "",
    username: c?.username ?? "",
    sort_order: c?.sort_order != null ? String(c.sort_order) : "1",
    password: "",
    login_extra_params: c?.login_extra_params ?? "",
    credential_mode: c?.credential_mode ?? "password",
    balance_threshold: c?.balance_threshold != null ? String(c.balance_threshold) : "0",
    recharge_multiplier: c?.recharge_multiplier != null ? String(c.recharge_multiplier) : "",
    recharge_multiplier_mode: rechargeMultiplierMode,
    use_usage_stats_for_costs: c?.use_usage_stats_for_costs ?? false,
    monitor_enabled: c?.monitor_enabled ?? true,
    only_created_key_groups_enabled: c?.only_created_key_groups_enabled ?? false,
    skip_rates_on_auto_refresh: c?.skip_rates_on_auto_refresh ?? false,
    turnstile_enabled: c?.turnstile_enabled ?? false,
    ignore_announcements: c?.ignore_announcements ?? false,
    subscription_enabled: c?.subscription_enabled ?? false,
    proxy_enabled: c?.proxy_enabled ?? false,
    captcha_config_id: c?.captcha_config_id != null ? String(c.captcha_config_id) : "",
    tags: normalizeTags(c?.tags ?? []),
    tag_input: "",
    notes: c?.notes ?? "",
  }
}

/** tokenFieldValue 取某个 token 字段的输入值（去掉首尾空白）。 */
function tokenFieldValue(values: Record<string, string>, key: string): string {
  return (values[key] ?? "").trim()
}

/**
 * buildTokenCredential 把插件声明的 token 字段序列化成后端期望的 JSON 字符串。
 * 字段 key 与 backend/connector 插件注册的 TokenFields 对齐，只提交非空字段。
 */
function buildTokenCredential(
  fields: ConnectorTokenField[],
  values: Record<string, string>,
): string {
  const out: Record<string, string> = {}
  for (const field of fields) {
    const value = tokenFieldValue(values, field.key)
    if (value) out[field.key] = value
  }
  return JSON.stringify(out)
}

/** validateTokenValues 前端预校验：required 字段必填、one_of 组内至少填一个。 */
function validateTokenValues(
  fields: ConnectorTokenField[],
  values: Record<string, string>,
): string | null {
  const groups = new Map<string, ConnectorTokenField[]>()
  for (const field of fields) {
    if (field.required && !tokenFieldValue(values, field.key)) {
      return `请填写 ${field.label}`
    }
    if (field.one_of) {
      const list = groups.get(field.one_of) ?? []
      list.push(field)
      groups.set(field.one_of, list)
    }
  }
  for (const list of groups.values()) {
    if (!list.some((field) => tokenFieldValue(values, field.key))) {
      return `请至少填写 ${list.map((field) => field.label).join(" / ")} 中的一个`
    }
  }
  return null
}

export function ChannelFormDialog({ open, onOpenChange, channel }: ChannelFormDialogProps) {
  const [form, setForm] = useState<FormState>(() => initialState(channel))
  const [tokenValues, setTokenValues] = useState<Record<string, string>>({})
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const refresh = useTriggerRefresh()
  const captchas = useCaptchaConfigs(open)
  const plugins = useConnectorPlugins()

  // 打开 / 切换目标渠道时重置表单。
  useEffect(() => {
    if (open) {
      setForm(initialState(channel))
      setTokenValues({})
      setError(null)
    }
  }, [open, channel])

  const pluginList = plugins.data ?? []
  const currentPlugin = pluginList.find((p) => p.type === form.type)
  const tokenFields = currentPlugin?.token_fields ?? []
  const canSubscription = currentPlugin?.capabilities.includes("subscription") ?? false
  const canAPIKeys = currentPlugin?.capabilities.includes("api_keys") ?? false
  const canTurnstile = currentPlugin?.capabilities.includes("turnstile") ?? false
  const canLoginExtraParams = currentPlugin?.capabilities.includes("login_extra_params") ?? false
  const canAnnouncements = currentPlugin?.capabilities.includes("announcements") ?? false
  const canRates = currentPlugin?.capabilities.includes("rates") ?? false
  const canUsageStatsCosts = currentPlugin?.capabilities.includes("usage_stats_costs") ?? false

  // 插件列表加载后：新建渠道默认选中第一个可用插件；编辑渠道回填 user_id 字段。
  useEffect(() => {
    if (!open) return
    if (!channel && pluginList.length > 0 && !pluginList.some((p) => p.type === form.type)) {
      setForm((prev) => ({ ...prev, type: pluginList[0].type }))
      return
    }
    if (!channel || !currentPlugin) return
    const userIDField = tokenFields.find((field) => field.target === "user_id")
    const userID = (channel.user_id ?? "").trim()
    if (!userIDField || !userID) return
    setTokenValues((prev) =>
      prev[userIDField.key] ? prev : { ...prev, [userIDField.key]: userID },
    )
  }, [open, channel, pluginList, currentPlugin, tokenFields, form.type])

  const isEdit = !!channel
  const isTokenMode = form.credential_mode === "token"
  // 编辑模式下，若 credential_mode 没变，token / password 都可以留空表示不修改。
  const modeChanged = isEdit && form.credential_mode !== (channel?.credential_mode ?? "password")
  const notesLength = charLength(form.notes.trim())

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    setError(null)
    setSubmitting(true)
    try {
      const threshold = Number(form.balance_threshold)
      if (!Number.isFinite(threshold) || threshold < 0) {
        throw new Error("余额阈值必须是非负数")
      }
      const sortOrder = Number(form.sort_order)
      if (!Number.isInteger(sortOrder)) {
        throw new Error("排序必须是整数")
      }
      let rechargeMultiplier = 0
      const rechargeMultiplierText = form.recharge_multiplier.trim()
      if (rechargeMultiplierText) {
        rechargeMultiplier = Number(rechargeMultiplierText)
        if (!Number.isFinite(rechargeMultiplier) || rechargeMultiplier <= 0) {
          throw new Error("充值倍率必须大于 0，或留空跟随上游")
        }
      }
      // 输入框里未确认的文本也算作标签
      const tags = normalizeTags([...form.tags, form.tag_input])
      if (tags.length > MAX_TAGS) {
        throw new Error(`标签最多 ${MAX_TAGS} 个`)
      }
      const longTag = tags.find((tag) => charLength(tag) > MAX_TAG_LENGTH)
      if (longTag) {
        throw new Error(`单个标签最多 ${MAX_TAG_LENGTH} 个字符：${longTag}`)
      }
      const notes = form.notes.trim()
      if (notesLength > MAX_NOTES_LENGTH) {
        throw new Error(`备注最多 ${MAX_NOTES_LENGTH} 个字符`)
      }
      const loginExtraParams = isTokenMode ? "" : form.login_extra_params.trim()
      if (loginExtraParams) {
        let parsed: unknown
        try {
          parsed = JSON.parse(loginExtraParams)
        } catch {
          throw new Error("附加表单参数 JSON 格式错误")
        }
        if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") {
          throw new Error("附加表单参数必须是 JSON 对象")
        }
      }

      // token 模式：按插件声明的字段序列化凭据；编辑时留空表示不修改。
      let tokenCredential = ""
      if (isTokenMode) {
        const filled = tokenFields.some(
          (field) =>
            field.target !== "user_id" && tokenFieldValue(tokenValues, field.key),
        )
        const userIDChanged = tokenFields
          .filter((field) => field.target === "user_id")
          .some(
            (field) =>
              tokenFieldValue(tokenValues, field.key) !== (channel?.user_id ?? "").trim(),
          )
        if (!isEdit || modeChanged || filled || userIDChanged) {
          if (tokenFields.length === 0) {
            throw new Error("当前渠道类型未声明 Token 字段，请改用账号密码模式")
          }
          const invalid = validateTokenValues(tokenFields, tokenValues)
          if (invalid) throw new Error(invalid)
          tokenCredential = buildTokenCredential(tokenFields, tokenValues)
        }
      }

      // 打码 provider 只在 password 模式 + 启用 Turnstile 时生效
      const useCaptcha = !isTokenMode && form.turnstile_enabled
      const captchaConfigID =
        useCaptcha && form.captcha_config_id ? Number(form.captcha_config_id) : null
      if (useCaptcha && captchaConfigID == null) {
        throw new Error("启用 Turnstile 时必须选择一个打码 provider")
      }

      // password 模式下的密码校验
      if (!isTokenMode) {
        if (!isEdit && !form.password) throw new Error("新建时必须填写密码")
        if (modeChanged && !form.password) throw new Error("切换到账号密码模式时必须填写密码")
      }

      if (isEdit) {
        const body: Record<string, unknown> = {
          name: form.name,
          site_url: form.site_url,
          username: form.username,
          sort_order: sortOrder,
          credential_mode: form.credential_mode,
          login_extra_params: loginExtraParams,
          balance_threshold: threshold,
          recharge_multiplier: rechargeMultiplier,
          recharge_multiplier_mode: form.recharge_multiplier_mode,
          use_usage_stats_for_costs: canUsageStatsCosts && form.use_usage_stats_for_costs,
          monitor_enabled: form.monitor_enabled,
          only_created_key_groups_enabled: form.only_created_key_groups_enabled,
          skip_rates_on_auto_refresh: canRates && form.skip_rates_on_auto_refresh,
          turnstile_enabled: !isTokenMode && canTurnstile && form.turnstile_enabled,
          ignore_announcements: form.ignore_announcements,
          subscription_enabled: canSubscription && form.subscription_enabled,
          proxy_enabled: form.proxy_enabled,
          captcha_config_id: captchaConfigID,
          tags,
          notes,
        }
        if (!isTokenMode && form.password) body.password = form.password
        if (isTokenMode && tokenCredential) body.token_credential = tokenCredential
        await apiFetch(`/channels/${channel!.id}`, {
          method: "PUT",
          body: JSON.stringify(body),
        })
      } else {
        await apiFetch(`/channels`, {
          method: "POST",
          body: JSON.stringify({
            name: form.name,
            type: form.type,
            site_url: form.site_url,
            username: form.username,
            sort_order: sortOrder,
            credential_mode: form.credential_mode,
            login_extra_params: loginExtraParams,
            password: isTokenMode ? "" : form.password,
            token_credential: isTokenMode ? tokenCredential : "",
            balance_threshold: threshold,
            recharge_multiplier: rechargeMultiplier,
            recharge_multiplier_mode: form.recharge_multiplier_mode,
            use_usage_stats_for_costs: canUsageStatsCosts && form.use_usage_stats_for_costs,
            monitor_enabled: form.monitor_enabled,
            only_created_key_groups_enabled: form.only_created_key_groups_enabled,
            skip_rates_on_auto_refresh: canRates && form.skip_rates_on_auto_refresh,
            turnstile_enabled: !isTokenMode && canTurnstile && form.turnstile_enabled,
            ignore_announcements: form.ignore_announcements,
            subscription_enabled: canSubscription && form.subscription_enabled,
            proxy_enabled: form.proxy_enabled,
            captcha_config_id: captchaConfigID,
            tags,
            notes,
          }),
        })
      }
      onOpenChange(false)
      refresh()
    } catch (e) {
      const err = e as Error
      setError(err.message || "保存失败")
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{isEdit ? "编辑渠道" : "新增渠道"}</DialogTitle>
          <DialogDescription>
            {isEdit ? "修改后会清空已缓存的登录会话。" : "添加上游账号，开启监控后将按计划自动登录。"}
          </DialogDescription>
        </DialogHeader>

        <form onSubmit={handleSubmit} className="space-y-3">
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_120px]">
            <div className="space-y-1.5">
              <Label htmlFor="name">渠道名</Label>
              <Input
                id="name"
                value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })}
                required
                disabled={submitting}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="sort-order">排序</Label>
              <Input
                id="sort-order"
                type="number"
                step="1"
                value={form.sort_order}
                onChange={(e) => setForm({ ...form, sort_order: e.target.value })}
                disabled={submitting}
              />
            </div>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="type">类型</Label>
            <Select
              value={form.type}
              onValueChange={(v) => setForm({ ...form, type: v as ChannelType })}
              disabled={isEdit || submitting}
            >
              <SelectTrigger id="type" className="w-full">
                <SelectValue placeholder={plugins.loading ? "加载插件…" : "选择系统类型"} />
              </SelectTrigger>
              <SelectContent>
                {pluginList.length > 0 ? (
                  pluginList.map((plugin) => (
                    <SelectItem key={plugin.type} value={plugin.type}>
                      {plugin.name}
                    </SelectItem>
                  ))
                ) : (
                  <>
                    <SelectItem value="sub2api">Sub2API</SelectItem>
                    <SelectItem value="newapi">NewAPI</SelectItem>
                  </>
                )}
              </SelectContent>
            </Select>
            {isEdit ? (
              <p className="text-[11px] text-muted-foreground">类型创建后不可修改</p>
            ) : currentPlugin?.description ? (
              <p className="text-[11px] text-muted-foreground">{currentPlugin.description}</p>
            ) : null}
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="site_url">站点地址</Label>
            <Input
              id="site_url"
              placeholder="https://example.com"
              value={form.site_url}
              onChange={(e) => setForm({ ...form, site_url: e.target.value })}
              required
              disabled={submitting}
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="channel-tags">标签（可选）</Label>
            <TagsInput
              id="channel-tags"
              tags={form.tags}
              input={form.tag_input}
              onChange={(tags, tag_input) => setForm({ ...form, tags, tag_input })}
              disabled={submitting}
            />
            <p className="text-[11px] text-muted-foreground">
              {`回车或逗号确认，最多 ${MAX_TAGS} 个，每个不超过 ${MAX_TAG_LENGTH} 个字符`}
            </p>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="channel-notes">备注（可选）</Label>
            <Textarea
              id="channel-notes"
              placeholder="如：主力渠道，晚高峰较慢"
              value={form.notes}
              onChange={(e) => setForm({ ...form, notes: e.target.value })}
              rows={2}
              className="field-sizing-fixed text-sm"
              disabled={submitting}
            />
            {notesLength > MAX_NOTES_LENGTH * 0.9 ? (
              <p
                className={cn(
                  "text-right text-[11px]",
                  notesLength > MAX_NOTES_LENGTH ? "text-destructive" : "text-muted-foreground",
                )}
              >
                {`${notesLength} / ${MAX_NOTES_LENGTH}`}
              </p>
            ) : null}
          </div>

          {/* 凭据类型 toggle */}
          <div className="space-y-1.5">
            <Label>凭据类型</Label>
            <div className="grid grid-cols-1 gap-2 rounded-lg border border-border p-1 sm:grid-cols-2">
              <button
                type="button"
                disabled={submitting}
                onClick={() => setForm({ ...form, credential_mode: "password" })}
                className={cn(
                  "rounded-md px-3 py-1.5 text-xs font-medium transition-colors",
                  !isTokenMode
                    ? "bg-foreground text-background"
                    : "text-muted-foreground hover:bg-muted",
                )}
              >
                账号密码
              </button>
              <button
                type="button"
                disabled={submitting}
                onClick={() => setForm({ ...form, credential_mode: "token" })}
                className={cn(
                  "rounded-md px-3 py-1.5 text-xs font-medium transition-colors",
                  isTokenMode
                    ? "bg-foreground text-background"
                    : "text-muted-foreground hover:bg-muted",
                )}
              >
                Token (跳过登录)
              </button>
            </div>
            <p className="text-[11px] text-muted-foreground">
              {isTokenMode
                ? "粘贴浏览器里已登录后的 Token / Cookie。失效时需要手动重新粘贴。"
                : "提供账号密码，系统自动登录并续期。可能需要配打码 provider。"}
            </p>
          </div>

          {/* —— password 模式字段 —— */}
          {!isTokenMode ? (
            <>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <Label htmlFor="username">账号 / 邮箱</Label>
                  <Input
                    id="username"
                    value={form.username}
                    onChange={(e) => setForm({ ...form, username: e.target.value })}
                    required
                    disabled={submitting}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="password">
                    {isEdit ? "新密码 (留空不变)" : "密码"}
                  </Label>
                  <Input
                    id="password"
                    type="password"
                    value={form.password}
                    onChange={(e) => setForm({ ...form, password: e.target.value })}
                    required={!isEdit || modeChanged}
                    disabled={submitting}
                  />
                </div>
              </div>
            </>
          ) : null}

          {/* —— token 模式字段 —— */}
          {isTokenMode ? (
            <>
              <div className="space-y-1.5">
                <Label htmlFor="username-display">账号 / 邮箱（可选）</Label>
                <Input
                  id="username-display"
                  placeholder="如：worry@example.com"
                  value={form.username}
                  onChange={(e) => setForm({ ...form, username: e.target.value })}
                  disabled={submitting}
                />
                <p className="text-[11px] text-muted-foreground">
                  仅作展示，不参与鉴权
                </p>
              </div>

              {tokenFields.length > 0 ? (
                <>
                  <TokenFieldsEditor
                    fields={tokenFields}
                    values={tokenValues}
                    onChange={(key, value) =>
                      setTokenValues((prev) => ({ ...prev, [key]: value }))
                    }
                    isEdit={isEdit}
                    submitting={submitting}
                    renderHelp={(field) => {
                      if (currentPlugin?.type === "newapi") {
                        if (field.key === "cookie") return <NewAPITokenHelp kind="cookie" />
                        if (field.key === "access_token")
                          return <NewAPITokenHelp kind="access_token" />
                      }
                      if (currentPlugin?.type === "sub2api" && field.key === "access_token") {
                        return <Sub2APITokenHelp />
                      }
                      return null
                    }}
                  />
                  {currentPlugin?.token_hint ? (
                    <p className="text-[11px] text-muted-foreground">{currentPlugin.token_hint}</p>
                  ) : null}
                </>
              ) : (
                <p className="text-[11px] text-muted-foreground">
                  当前插件未声明 Token 字段，请使用账号密码模式。
                </p>
              )}
            </>
          ) : null}

          {!isTokenMode && canLoginExtraParams ? (
            <div className="space-y-1.5">
              <Label htmlFor="login-extra-params">附加表单参数</Label>
              <Textarea
                id="login-extra-params"
                placeholder='{"device_id":"xxx"}'
                value={form.login_extra_params}
                onChange={(e) => setForm({ ...form, login_extra_params: e.target.value })}
                rows={3}
                className="field-sizing-fixed text-xs font-mono"
                disabled={submitting}
              />
              <p className="text-[11px] text-muted-foreground">
                用于非标准 Sub2API / NewAPI 魔改版增加的登录参数字段。
              </p>
            </div>
          ) : null}

          <div className="space-y-1.5">
            <Label htmlFor="threshold">余额阈值（低于此值发告警，0 = 不告警）</Label>
            <Input
              id="threshold"
              type="number"
              step="0.01"
              min="0"
              value={form.balance_threshold}
              onChange={(e) => setForm({ ...form, balance_threshold: e.target.value })}
              disabled={submitting}
            />
          </div>

          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="recharge-multiplier">充值倍率（留空 = 跟随上游）</Label>
              <Input
                id="recharge-multiplier"
                type="number"
                step="0.0001"
                min="0"
                value={form.recharge_multiplier}
                onChange={(e) => setForm({ ...form, recharge_multiplier: e.target.value })}
                disabled={submitting}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="recharge-multiplier-mode">换算方式</Label>
              <Select
                value={form.recharge_multiplier_mode}
                onValueChange={(v) =>
                  setForm({ ...form, recharge_multiplier_mode: v as RechargeMultiplierMode })
                }
                disabled={submitting}
              >
                <SelectTrigger id="recharge-multiplier-mode" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="divide">余额 / 倍率</SelectItem>
                  <SelectItem value="multiply">余额 × 倍率</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
            <div>
              <p className="text-sm font-medium">启用监控</p>
              <p className="text-xs text-muted-foreground">关闭后调度器不会扫描此渠道</p>
            </div>
            <Switch
              checked={form.monitor_enabled}
              onCheckedChange={(v) => setForm({ ...form, monitor_enabled: v })}
              disabled={submitting}
            />
          </div>

          {canUsageStatsCosts ? (
            <div className="flex items-center justify-between gap-3 rounded-lg border border-border px-3 py-2">
              <div>
                <Label htmlFor="use-usage-stats-for-costs">通过使用记录 API 获取消费</Label>
                <p className="text-xs text-muted-foreground">
                  适用于消费同步报 404 的站点。今日按香港时区统计，累计按上游可用使用记录统计；继续应用充值倍率换算
                </p>
              </div>
              <Switch
                id="use-usage-stats-for-costs"
                checked={form.use_usage_stats_for_costs}
                onCheckedChange={(v) => setForm({ ...form, use_usage_stats_for_costs: v })}
                disabled={submitting}
              />
            </div>
          ) : null}

          {canRates ? (
            <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
              <div>
                <p className="text-sm font-medium">{"不采集分组倍率（只取额度）"}</p>
                <p className="text-xs text-muted-foreground">
                  {"开启后定时扫描和卡片上的「同步」都不再拉分组倍率（顺带不再同步公告），只更新余额、消费与订阅用量；已存的倍率保持原样可读"}
                </p>
              </div>
              <Switch
                checked={form.skip_rates_on_auto_refresh}
                onCheckedChange={(v) => setForm({ ...form, skip_rates_on_auto_refresh: v })}
                disabled={submitting || !form.monitor_enabled}
              />
            </div>
          ) : null}

          {canAPIKeys ? (
            <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
              <div>
                <p className="text-sm font-medium">仅显示已创建密钥的分组</p>
                <p className="text-xs text-muted-foreground">
                  开启后渠道卡片 / 分组对话框 / 倍率变动通知只关注已创建密钥的分组；网关与通知设置仍可看全部分组
                </p>
              </div>
              <Switch
                checked={form.only_created_key_groups_enabled}
                onCheckedChange={(v) => setForm({ ...form, only_created_key_groups_enabled: v })}
                disabled={submitting}
              />
            </div>
          ) : null}

          {canAnnouncements ? (
            <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
              <div>
                <p className="text-sm font-medium">忽略公告</p>
                <p className="text-xs text-muted-foreground">开启后不会拉取该渠道公告</p>
              </div>
              <Switch
                checked={form.ignore_announcements}
                onCheckedChange={(v) => setForm({ ...form, ignore_announcements: v })}
                disabled={submitting}
              />
            </div>
          ) : null}

          {canSubscription ? (
            <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
              <div>
                <p className="text-sm font-medium">启用订阅购买</p>
                <p className="text-xs text-muted-foreground">开启后充值弹窗显示订阅购买</p>
              </div>
              <Switch
                checked={form.subscription_enabled}
                onCheckedChange={(v) => setForm({ ...form, subscription_enabled: v })}
                disabled={submitting}
              />
            </div>
          ) : null}

          <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
            <div>
              <p className="text-sm font-medium">启用代理 IP</p>
              <p className="text-xs text-muted-foreground">全局代理启用后，该渠道上游请求走系统代理配置</p>
            </div>
            <Switch
              checked={form.proxy_enabled}
              onCheckedChange={(v) => setForm({ ...form, proxy_enabled: v })}
              disabled={submitting}
            />
          </div>

          {/* Turnstile / 打码：token 模式下整段不展示 */}
          {!isTokenMode && canTurnstile ? (
            <>
              <div className="flex items-center justify-between rounded-lg border border-border px-3 py-2">
                <div>
                  <p className="text-sm font-medium">Turnstile 人机校验</p>
                  <p className="text-xs text-muted-foreground">站点开启 Cloudflare Turnstile 时打开</p>
                </div>
                <Switch
                  checked={form.turnstile_enabled}
                  onCheckedChange={(v) => setForm({ ...form, turnstile_enabled: v })}
                  disabled={submitting}
                />
              </div>

              {form.turnstile_enabled ? (
                <div className="space-y-1.5">
                  <Label htmlFor="captcha-config">打码 provider</Label>
                  <Select
                    value={form.captcha_config_id}
                    onValueChange={(v) => setForm({ ...form, captcha_config_id: v })}
                    disabled={submitting}
                  >
                    <SelectTrigger id="captcha-config" className="w-full">
                      <SelectValue
                        placeholder={
                          captchas.data && captchas.data.length > 0
                            ? "选择 provider"
                            : "先到底部 [验证码服务] 卡片新增"
                        }
                      />
                    </SelectTrigger>
                    <SelectContent>
                      {(captchas.data ?? [])
                        .filter((c) => c.enabled)
                        .map((c) => (
                          <SelectItem key={c.id} value={String(c.id)}>
                            {c.name}
                          </SelectItem>
                        ))}
                    </SelectContent>
                  </Select>
                  <p className="text-[11px] text-muted-foreground">
                    {"siteKey 会自动从上游公开接口拉取，无需在此填写。"}
                  </p>
                </div>
              ) : null}
            </>
          ) : null}

          {error ? (
            <p className="text-sm text-destructive" role="alert">
              {error}
            </p>
          ) : null}

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={submitting}>
              取消
            </Button>
            <Button type="submit" disabled={submitting}>
              {submitting ? "保存中…" : "保存"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

interface TagsInputProps {
  id: string
  tags: string[]
  input: string
  onChange: (tags: string[], input: string) => void
  disabled?: boolean
}

/**
 * TagsInput 标签输入：回车、半角 "," 或全角 "，" 把当前文本确认为 chip；
 * 输入框为空时按 Backspace 删除最后一个 chip；chip 上的 × 可用键盘聚焦后回车删除。
 * 超长的 chip 标红提示，真正的限制在提交时统一校验。
 */
function TagsInput({ id, tags, input, onChange, disabled }: TagsInputProps) {
  const inputRef = useRef<HTMLInputElement>(null)

  function handleInputChange(value: string) {
    // 逗号来自键盘、输入法或粘贴都走这里：最后一个逗号之前的内容确认为 chip，其余留在输入框
    const parts = value.split(/[,，]/)
    if (parts.length === 1) {
      onChange(tags, value)
      return
    }
    const rest = parts.pop() ?? ""
    onChange(normalizeTags([...tags, ...parts]), rest)
  }

  function handleKeyDown(e: KeyboardEvent<HTMLInputElement>) {
    // 输入法组字过程中的回车 / 退格交给输入法处理（Safari 确认候选词时 isComposing 已为 false，靠 keyCode 229 识别）
    if (e.nativeEvent.isComposing || e.keyCode === 229) return
    if (e.key === "Enter") {
      // 避免在标签框里回车直接提交整个表单
      e.preventDefault()
      if (input.trim()) onChange(normalizeTags([...tags, input]), "")
      return
    }
    if (e.key === "Backspace" && input === "" && tags.length > 0) {
      e.preventDefault()
      onChange(tags.slice(0, -1), input)
    }
  }

  function removeTag(index: number) {
    onChange(
      tags.filter((_, i) => i !== index),
      input,
    )
    inputRef.current?.focus()
  }

  return (
    <div
      className={cn(
        "flex min-h-9 w-full flex-wrap items-center gap-1.5 rounded-md border border-input bg-transparent px-2 py-1.5 shadow-xs transition-[color,box-shadow] dark:bg-input/30",
        "focus-within:border-ring focus-within:ring-[3px] focus-within:ring-ring/50",
        disabled && "cursor-not-allowed opacity-50",
      )}
      onClick={(e) => {
        if (e.target === e.currentTarget) inputRef.current?.focus()
      }}
    >
      {tags.map((tag, index) => {
        const tooLong = charLength(tag) > MAX_TAG_LENGTH
        return (
          <span
            key={tag}
            className={cn(
              "inline-flex max-w-full items-center gap-0.5 rounded bg-muted py-0.5 pr-0.5 pl-1.5 text-xs text-foreground ring-1 ring-inset",
              tooLong ? "text-destructive ring-destructive/50" : "ring-border",
            )}
            title={tooLong ? `超过 ${MAX_TAG_LENGTH} 个字符：${tag}` : tag}
          >
            {/* 允许折行而不是 truncate：nowrap 会把弹窗撑宽 */}
            <span className="min-w-0 wrap-anywhere">{tag}</span>
            <button
              type="button"
              onClick={() => removeTag(index)}
              disabled={disabled}
              aria-label={`移除标签 ${tag}`}
              className="inline-flex shrink-0 items-center justify-center rounded-sm p-0.5 text-muted-foreground outline-none hover:bg-background hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 disabled:pointer-events-none"
            >
              <X className="size-3" />
            </button>
          </span>
        )
      })}
      <input
        ref={inputRef}
        id={id}
        value={input}
        onChange={(e) => handleInputChange(e.target.value)}
        onKeyDown={handleKeyDown}
        placeholder={tags.length === 0 ? "输入后按回车添加" : ""}
        disabled={disabled}
        autoComplete="off"
        className="h-6 min-w-24 flex-1 bg-transparent text-base outline-none placeholder:text-muted-foreground disabled:cursor-not-allowed md:text-sm"
      />
    </div>
  )
}

interface TokenFieldsEditorProps {
  fields: ConnectorTokenField[]
  values: Record<string, string>
  onChange: (key: string, value: string) => void
  isEdit?: boolean
  submitting?: boolean
  renderHelp?: (field: ConnectorTokenField) => ReactNode
}

/**
 * TokenFieldsEditor 按插件声明的 token_fields 动态渲染输入项。
 * 编辑模式下所有字段都可以留空表示「不修改」，提交时由父组件决定是否整体覆盖。
 */
function TokenFieldsEditor({
  fields,
  values,
  onChange,
  isEdit,
  submitting,
  renderHelp,
}: TokenFieldsEditorProps) {
  return (
    <>
      {fields.map((field) => {
        const id = `token-field-${field.key}`
        const placeholder = isEdit
          ? "留空则不修改"
          : (field.placeholder ?? "")
        return (
          <div key={field.key} className="space-y-1.5">
            <div className="flex items-center justify-between gap-2">
              <Label htmlFor={id}>
                {field.label}
                {field.required ? <span className="text-destructive"> *</span> : null}
              </Label>
              {renderHelp?.(field)}
            </div>
            {field.secret ? (
              <Textarea
                id={id}
                value={values[field.key] ?? ""}
                onChange={(e) => onChange(field.key, e.target.value)}
                placeholder={placeholder}
                rows={3}
                className="field-sizing-fixed text-xs font-mono"
                disabled={submitting}
              />
            ) : (
              <Input
                id={id}
                value={values[field.key] ?? ""}
                onChange={(e) => onChange(field.key, e.target.value)}
                placeholder={placeholder}
                disabled={submitting}
              />
            )}
            {field.help ? (
              <p className="text-[11px] text-muted-foreground">{field.help}</p>
            ) : null}
          </div>
        )
      })}
    </>
  )
}


function NewAPITokenHelp({ kind = "cookie" }: { kind?: "cookie" | "access_token" }) {
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="inline-flex items-center gap-1 text-[11px] text-muted-foreground hover:text-foreground"
        >
          <HelpCircle className="size-3" />
          如何获取？
        </button>
      </PopoverTrigger>
      <PopoverContent className="w-80 text-xs" align="end">
        {kind === "access_token" ? (
          <>
            <p className="font-medium text-foreground">获取系统访问令牌</p>
            <ol className="mt-1 ml-4 list-decimal space-y-0.5 text-muted-foreground">
              <li>在浏览器登录 NewAPI 站点，进入「个人设置」页</li>
              <li>找到「生成的系统访问令牌」区块</li>
              <li>点击生成并复制这串 32 位令牌（即 user.access_token）</li>
            </ol>
            <p className="mt-2 text-muted-foreground">
              connector 用 <span className="font-mono text-foreground">Authorization</span> 头携带此令牌，不再依赖 Cookie 续期。
            </p>
          </>
        ) : (
          <>
            <p className="font-medium text-foreground">获取 Cookie</p>
            <ol className="mt-1 ml-4 list-decimal space-y-0.5 text-muted-foreground">
              <li>在浏览器登录 NewAPI 站点</li>
              <li>按 F12 打开 DevTools，切到 Application / 存储 标签</li>
              <li>左侧 Cookies 选中站点域名</li>
              <li>复制 <span className="font-mono text-foreground">session</span> 字段值，格式：<span className="font-mono">session=xxxxx</span></li>
            </ol>
          </>
        )}
        <p className="mt-2 font-medium text-foreground">获取 User ID</p>
        <p className="mt-1 text-muted-foreground">
          登录 NewAPI 后到「个人设置」页查看用户 ID，或到 Application / Local Storage 里复制 <span className="font-mono">uid</span>。
        </p>
      </PopoverContent>
    </Popover>
  )
}

function Sub2APITokenHelp() {
  return (
    <Popover>
      <PopoverTrigger asChild>
        <button
          type="button"
          className="inline-flex items-center gap-1 text-[11px] text-muted-foreground hover:text-foreground"
        >
          <HelpCircle className="size-3" />
          如何获取？
        </button>
      </PopoverTrigger>
      <PopoverContent className="w-96 text-xs" align="end">
        <p className="font-medium text-foreground">获取 Access Token / Refresh Token</p>
        <ol className="mt-1 ml-4 list-decimal space-y-0.5 text-muted-foreground">
          <li>在浏览器登录 Sub2API 站点</li>
          <li>按 F12 打开 DevTools，切到 Application / 存储 标签</li>
          <li>左侧 Local Storage 选中站点域名</li>
          <li>复制 <span className="font-mono text-foreground">auth_token</span> 或 <span className="font-mono text-foreground">access_token</span></li>
          <li>同时复制 <span className="font-mono text-foreground">refresh_token</span>（如果站点已启用刷新令牌）</li>
        </ol>
        <p className="mt-2 font-medium text-foreground">登录后特征</p>
        <p className="mt-1 text-muted-foreground">
          标准 Sub2API 登录接口是 <span className="font-mono">/api/v1/auth/login</span>，成功后返回 <span className="font-mono">access_token</span>、<span className="font-mono">refresh_token</span> 和 <span className="font-mono">expires_in</span>，前端会把 access token 保存为 <span className="font-mono">auth_token</span>。
        </p>
        <p className="mt-2 text-[11px] text-muted-foreground">
          也可以在 Network 标签里找任意已登录接口的 <span className="font-mono">Authorization</span> 头，去掉 <span className="font-mono">Bearer </span> 前缀。
        </p>
      </PopoverContent>
    </Popover>
  )
}
