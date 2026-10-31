# 渠道插件系统

UpstreamOps 的渠道（connector）按「编译期插件」组织：每个上游平台是一个独立的 Go 包，
只有在 `backend/plugins/plugins.go` 的 import 清单里登记过的插件才会编译进二进制。
这样接入一个第三方站点时，它的专用代码不会混进 `newapi` / `sub2api` 的主流程；
不想要某个插件时，删掉一行 import 重新编译即可。

## 目录结构

| 位置 | 作用 |
| --- | --- |
| `backend/connector/plugin.go` | 插件注册表、`PluginDescriptor`、`Capability`、`TokenField`、token 凭据解析 |
| `backend/connector/connector.go` | `Connector` 接口（19 个方法）与公共结果类型 |
| `backend/connector/<plugin>/` | 单个上游的实现，例如 `aiapi`、`newapi`、`sub2api` |
| `backend/plugins/plugins.go` | **编译期插件清单**：唯一的 blank import 列表 |
| `GET /api/connector-plugins` | 前端读取已编译插件的能力与 token 字段（前端不再写死渠道类型） |

## 删减插件

编辑 `backend/plugins/plugins.go`，删掉对应 import 行后重新编译：

```go
package plugins

import (
	_ "github.com/bejix/upstream-ops/backend/connector/aiapi"
	_ "github.com/bejix/upstream-ops/backend/connector/newapi"
	_ "github.com/bejix/upstream-ops/backend/connector/sub2api"
)
```

删除后该类型不再出现在「新增渠道」的类型下拉里，旧渠道记录会保留但无法同步
（`connector "xxx" is not registered`）。包目录可以保留，也可以一并删除。

## 新增插件

1. 在 `backend/connector/<plugin>/` 新建包，实现 `connector.Connector` 的 19 个方法。
   未实现的能力返回 `connector.Unsupported("充值")` 这类错误即可，不要返回空实现。
2. 在 `init()` 里注册描述符：

```go
func init() {
	connector.RegisterPlugin(connector.Plugin{
		Descriptor: connector.PluginDescriptor{
			Type:         "example",
			Name:         "Example",
			Description:  "Example 平台：余额 / 消费 / 倍率",
			Capabilities: []connector.Capability{
				connector.CapLogin, connector.CapTokenCredential,
				connector.CapBalance, connector.CapCosts, connector.CapRates,
				connector.CapAnnouncements,
			},
			TokenFields: []connector.TokenField{
				{
					Key: "cookie", Label: "站点 Cookie", Required: true,
					Secret: true, Target: "cookie", Help: "浏览器登录后复制",
				},
			},
			TokenHint: "从浏览器复制 Cookie",
		},
		Factory: func() connector.Connector { return New() },
	})
}
```

3. 在 `backend/plugins/plugins.go` 加一行 blank import。
4. 前端与后端的能力判断会自动生效：
   - 类型下拉、token 凭据表单按 `token_fields` 渲染；
   - 充值 / 兑换 / 订阅 / API Key 等操作入口按 `capabilities` 显示或隐藏；
   - 后端对未声明能力的请求返回「渠道类型 xxx 不支持…」。

## 能力清单

| Capability | 含义 |
| --- | --- |
| `login` / `token_credential` | 支持账号密码登录 / token 凭据 |
| `turnstile` / `login_extra_params` | 登录需要 Turnstile / 额外参数 |
| `balance` / `costs` | 余额 / 消费同步 |
| `rates` / `announcements` | 分组倍率 / 公告同步 |
| `redeem` / `recharge` | 兑换码 / 充值 |
| `subscription` / `subscription_usage` | 订阅购买 / 订阅用量告警 |
| `api_keys` | 上游 API Key 管理 |

## TokenField

token 凭据保存为一段 JSON（加密入库），字段规则完全由插件声明：

| 字段 | 说明 |
| --- | --- |
| `key` / `label` / `placeholder` / `help` | 前端表单展示 |
| `required` | 必填 |
| `secret` | 前端用密码框/多行框渲染 |
| `target` | 落到 `AuthSession` 的位置：`cookie` / `access_token` / `refresh_token` / `user_id` |
| `one_of` | 同组字段至少填一个（如 NewAPI 的 Cookie 与系统访问令牌二选一） |

`Plugin.ValidateToken` 可选，用于额外语义校验（字段互斥等）。

## AIApi（cn.aiapi.bot）

`backend/connector/aiapi/` 是首个「第三方站点」插件，能力：登录、token 凭据、余额、消费、
分组倍率、公告；**不支持**充值 / 订阅 / 兑换 / API Key 管理（调用会返回明确错误）。

- 登录：`POST /api/auth/login`（账号密码，无需 Turnstile），会话是 7 天有效的 `site-session` Cookie。
- token 模式：只需粘贴 `site-session=...` 这一段 Cookie。
- 余额：`GET /api/me/commerce/get-user-asset-summary` 的 `assetReadModel.walletAvailableUsd`。
- 消费：今日 = `GET /api/me/usage/history/summary`（按 Asia/Shanghai 当日）；
  累计 = `GET /api/me/usage/activity-calendar?range=365d` 每日 `costUsd` 求和
  （站点只提供 365 天日历，超过一年的消费不计入，属于近似值）。
- 分组倍率：`GET /api/me/routing-groups` 的 `billingMultiplier`。
- 公告：`GET /api/me/announcements`（只同步 `status=published`）。

## 验证

```bash
go build ./...
go test ./...
curl -s http://127.0.0.1:8418/api/connector-plugins | jq
# 前端
cd frontend && npx tsc --noEmit && pnpm build
```
