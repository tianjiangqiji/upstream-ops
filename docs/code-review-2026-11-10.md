# 代码审核与今日收益弹窗修复记录（2026-11-10）

本次按要求只修复「今日收益」窗口报错时的横向溢出。下列其他问题均为审核意见，未修改业务代码，待你决定下一步。

## 已完成的修复

修改文件：[profit-dialog.tsx](../frontend/components/monitor/profit-dialog.tsx)，第 346、392、397、424、446 行，共 5 处样式调整。

- 允许弹窗主体在 Grid 内缩小，避免长内容撑宽整列。
- 步骤错误、流程错误及固定宽度的渠道名称列允许连续字符串换行，保留原有换行符。
- 限制失败渠道名称容器宽度，保持名称截断及按钮换行有效。

没有修改公共 Dialog、收益计算、请求流程、重试逻辑或其他页面。错误全文仍可阅读，没有用隐藏内容的方式掩盖溢出。

浏览器使用模拟接口验证，没有连接真实上游。测试内容包含约 700 字符的连续 URL、中文多行错误、长渠道名称、失败渠道列表和正常收益结果。等待尺寸动画结束后的测量如下：

| 视口宽度 | 修复后的弹窗 clientWidth | 修复后的 scrollWidth | 在浏览器中恢复原换行/收缩规则后的 scrollWidth |
| --- | --- | --- | --- |
| 320 px | 294 px | 294 px | 4996 px |
| 375 px | 349 px | 349 px | 4996 px |
| 768 px | 670 px | 670 px | 5012 px |
| 1440 px | 670 px | 670 px | 5012 px |

四种尺寸下，弹窗左右边界均在视口内；带失败渠道列表时也没有横向滚动。正常结果仍显示 `100 - 5 - 10 = 85`。[窄屏错误截图](images/profit-dialog-error-2026-11-10-mobile.png)。

## 审核范围与方法

清点了 `frontend`、`backend`、`cmd`、`web` 内 286 份 Go、TS、TSX、CSS 文件，约 7.4 万非空行，包含现有测试；同时检查了构建、配置与部署入口。采用全仓静态检索、关键调用链人工追踪、现有测试和临时复现探针，未将第三方依赖及生成产物作为自有代码逐行审核。

| 范围 | 本次核查重点 |
| --- | --- |
| 前端页面、表单、公共组件 | 长文本布局、轮询、编辑草稿、异步响应、错误与加载状态 |
| 收益、渠道、监控 | 重试来源、取消、消费采集、登录凭据、渠道配置更新 |
| 网关管理与转发 | 密钥、源分组、费率、鉴权、IP 限制、HTTP 请求与模型列表 |
| 协议与用量 | Chat / Responses / Anthropic 转换路径及测试、Gemini 入口、计费持久化 |
| 存储、设置、定时任务、通知、验证码 | 事务边界、热更新、清理、重试与已有用例 |

以下标注「已复现」的后端问题使用独立临时 SQLite 数据库或内存 HTTP 请求验证；标注「静态确认」的结论有明确调用路径，但未进行真实服务集成测试。现有测试通过不代表这些未覆盖的场景正确。未进行真实支付、通知发送、生产数据库操作或外部上游端到端测试。

## 建议优先处理的问题

### R1 · P1 · 默认代理信任配置允许绕过网关 IP 限制（已复现）

位置：[main.go](../cmd/server/main.go) 第 165–166 行；[runtime_auth.go](../backend/gateway/runtime_auth.go) 第 30 行。

当 `trustedProxies` 留空时，代码没有调用 `SetTrustedProxies`，Gin 保留默认信任所有代理的行为。网关黑白名单又使用 `c.ClientIP()`。持有有效网关密钥的调用者直接连接服务时，可以伪造 `X-Forwarded-For`，使访问控制判断错误的地址。

复现：实际远端为 `198.51.100.12`，请求头声明 `203.0.113.99`，`ClientIP()` 返回了后者。

建议：默认明确禁用代理信任；只有配置了可信反向代理时才采信其转发地址，并处理 `SetTrustedProxies` 返回的错误。补充直连、可信代理、不可信代理的黑白名单用例。

### R2 · P1 · 编辑密钥信息可能回退已经累计的消费额度（已复现）

位置：[admin_keys.go](../backend/gateway/admin_keys.go) 第 57–90 行；[storage/gateway.go](../backend/storage/gateway.go) 第 267、302 行。

编辑操作先读取整条 Key，最后通过 `Save` 写回所有列。在读取与保存之间，转发请求可能已经通过 `AddQuotaUsed` 累加额度；随后保存名称、状态等信息会把旧 `quota_used` 覆盖回来，即使没有要求重置额度。

复现顺序：读取 `quota_used=0` → 模拟请求累计到 `3` → 只修改名称并保存 → 数据库额度重新变为 `0`。

建议：管理操作只更新明确可编辑的字段；额度重置作为单独的显式操作。相同的整行保存模式也出现在渠道配置更新中，应一起核对是否覆盖并发采集的余额、消费或错误状态。

### R3 · P1 · 修改路由源分组后仍保留旧分组的上游密钥（已复现）

位置：[storage/gateway.go](../backend/storage/gateway.go) 第 375–382 行；[runtime_forward.go](../backend/gateway/runtime_forward.go) 第 606–616 行。

`sameSource` 只比较来源类型、渠道 ID、Provider ID，没有比较源分组 ID / 名称。在同一监控渠道内把分组 A 改为 B，保存后仍保留 A 的密钥。运行时直接解密这把旧密钥，而显示的源分组和计算倍率已经来自 B，可能造成模型权限、实际扣费与展示金额不一致。

复现：路由源分组保存为 `group-B` 后，仍绑定 `key-for-A`，Key ID 仍为 `9`。

建议：将分组身份纳入来源一致性判断；分组变更后清除旧密钥绑定，并要求重新确保密钥，或完整验证并更新上游 Key 的所属分组。

### R4 · P2 · 系统设置轮询会覆盖未保存输入（静态确认）

位置：[settings-page.tsx](../frontend/app/settings-page.tsx) 第 127–135 行；[queries.ts](../frontend/lib/queries.ts) 第 399–400 行；[refresh-context.tsx](../frontend/lib/refresh-context.tsx) 第 23–34 行。

设置页每次拿到新的 `query.data` 就重新 `setForm`，而 `useSystemConfig()` 参与全局 30 秒轮询。用户编辑配置停留超过一个周期，或页面其他操作触发全局刷新时，输入会被服务端旧配置替换。

建议：首次加载时初始化草稿；之后用 dirty 状态保护编辑，仅在显式放弃、重新加载或保存成功时回填。网关自动刷新也会重载编辑草稿，建议同时明确其编辑期间的刷新规则。

### R5 · P2 · 关闭收益弹窗取消请求后仍发布新快照（已复现）

位置：[profit-dialog.tsx](../frontend/components/monitor/profit-dialog.tsx) 第 205–214 行；[profit/service.go](../backend/profit/service.go) 第 383–462 行。

前端关闭弹窗会 abort 请求，但后端把取消导致的消费采集错误当成普通供应商失败继续处理，并最终写入新收益快照。被取消的更新会成为「最新结果」，把尚未采集的渠道标为失败，可能混入沿用值或零值。

复现：以已取消的 context 调用已有快照的重试流程，返回 `nil` 错误，快照数由 `1` 增至 `2`。

建议：先明确产品预期。如果关闭即取消，应在开始、循环及落库前检查取消并保留上一份已完成快照；如果关闭只是隐藏进度，应把任务生命周期与浏览器连接分开管理。

### R6 · P2 · Provider 拉模型忽略代理与鉴权配置（静态确认）

位置：[admin_models.go](../backend/gateway/admin_models.go) 第 145–151 行；[runtime_public.go](../backend/gateway/runtime_public.go) 第 147–155 行。

管理侧同步模型和公开模型聚合把 Provider 转成只含地址的临时 Channel，随后只设置 Bearer 鉴权。Provider 的 `proxy_enabled`、`auth_style` 和额外请求头没有传入此路径。只接受 `x-api-key` 或必须经过代理的 Provider，可能正常转发请求，却始终无法同步模型；公开聚合还会跳过错误来源，得到不完整列表。

建议：模型拉取复用同一个上游目标及请求构建逻辑，统一处理 Provider 的代理、鉴权和额外请求头。测试至少覆盖 Bearer、x-api-key 和代理必需三种情况。

### R7 · P2 · HTTP 透传丢失 URL 查询参数（静态确认）

位置：[router.go](../backend/gateway/router.go) 中的图片批次 GET 路由；[runtime_stream.go](../backend/gateway/runtime_stream.go) 第 24–48 行。

路由传入固定 path，请求构建器仅拼接 `base + path`，没有传递原始 URL 的 query。例如 `GET /v1/images/batches?page=2&limit=10` 到上游后只剩 `/v1/images/batches`，分页与过滤参数失效。

建议：请求构建接口显式接收 URL 查询参数，按协议保留允许透传的参数，并排除不应向上游转交的网关凭据。

### R8 · P2 · Gemini 入口返回 Chat 响应，流式请求也被改成非流式（静态确认）

位置：[runtime_public.go](../backend/gateway/runtime_public.go) 第 309–388 行。

`HandleGeminiGenerate` 将文本输入改成 Chat 请求，固定 `stream=false`，随后直接调用 Chat 转发器；没有把响应转回 Gemini 的 `candidates/content/parts` 结构，也没有为 `streamGenerateContent` 输出 Gemini 流。标准 Gemini 客户端无法按其协议解析结果；图片、工具及 generationConfig 也没有完整映射。

建议：明确只支持哪些 Gemini 方法；为这些方法实现成对的请求和响应转换。未实现的方法应明确返回不支持，避免表面返回 200 却无法被客户端消费。

### R9 · P2 · 配置应用失败后可能已经部分生效（静态确认）

位置：[runtimeconfig/runtime.go](../backend/runtimeconfig/runtime.go) 第 148–185 行；[settings.go](../backend/api/settings.go) 第 76–83 行。

应用配置时先修改通知策略、代理和网关配置，之后才启动新 Scheduler。若 cron 表达式无效，新 Scheduler 启动失败，接口返回错误，但前面几个模块已使用新值，鉴权和旧 Scheduler 则保留原状态。保存接口也没有在写文件前完成这些运行参数的验证。

建议：先解析并验证整份候选配置、准备调度器，再统一切换；失败时保持原运行状态。文件保存也应在验证后进行，并采用临时文件加替换，避免半写文件。

## 其他审核意见

这些事项需要结合产品预期安排，不与上述已复现的数据错误混为一类：

- **会话撤销策略**：当前 HMAC token 只绑定用户名、过期时间和签名密钥。临时探针确认，同用户名更改密码而保留签名密钥时，旧 token 继续有效。这符合当前无状态实现，但不适合「改密即踢掉旧会话」的预期；如果需要该行为，应增加会话版本或独立撤销机制。位置：`backend/auth/auth.go:72`。
- **额度上限语义**：目前请求开始时检查已消费额度，结束后才累加；并发请求可能同时通过检查。需要决定额度是近似阈值还是硬上限，再选择预占/结算方案。位置：`backend/gateway/runtime_auth.go:33`、`runtime_usage.go:160`。
- **HTTP 连接复用**：网关每次创建新的 Transport，跨请求无法共享连接池；高流量下连接与 TLS 建连开销会增加。建议按代理/配置复用 Transport，并在配置替换时关闭旧空闲连接。本次未做压力测试。位置：`backend/gateway/runtime_http.go:55`。
- **维护成本**：设置页、网关页面、使用记录表、渠道卡片以及协议转换文件已达到千行以上。建议优先拆分「服务端数据 / 编辑草稿 / 操作状态」和协议消息转换，避免继续向大组件追加分支；此次 5 处样式修复没有引入新的结构层或业务分支。

## 验证记录

- `go test ./backend/...`：通过。
- 最终 `go test ./...`：待本轮最终执行结果确认。
- `tsc --noEmit`：通过；修复后的最终复查进行中。
- `vite build`：通过；修复后的最终构建进行中。现有主 JS 包约 1.35 MB，构建提示可考虑按页面拆包。
- 全仓 ESLint：有 3 处现有错误，本次未修复：
  - `frontend/components/gateway/gateway-page.tsx:727`：没有注册 `react-hooks/exhaustive-deps` 规则。
  - `frontend/components/gateway/gateway-providers-panel.tsx:101`：同上。
  - `frontend/components/gateway/usage-panel.tsx:317`：`usagePage` 未使用。
- 修复文件的 ESLint：待最终执行结果确认。
- 浏览器：四种宽度的错误消息、长名称失败列表均通过横向边界检查；375 px 正常收益结果检查通过。
- 临时探针：R1、R2、R3、R5 及会话行为均复现。断言预期安全/正确行为的探针因此失败；它们是审核证据，不是现有测试回归。探针源码已从工作树移除，仅使用临时数据，未修改生产业务或现有测试。

建议下一步先处理 R1–R3，再处理 R4–R5；R6–R9 可根据实际使用的 Provider、透传端点和配置操作频率安排。所有这些问题本次仅报告。
