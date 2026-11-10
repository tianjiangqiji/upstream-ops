# upstream-ops 项目长期记忆

## 仓库关系
- `origin` = https://github.com/tianjiangqiji/upstream-ops —— 下游 fork
- 上游 = https://github.com/bejix/upstream-ops —— 已加为 `upstream` remote
- 本地是**增强型 fork**：在上游基础上加了自己的改进，不只是裁剪上游功能
- 2026-09-26 起本地历史已**嫁接**到上游历史（此前是无关历史的单个 `first commit`）。
  以后同步上游：`git fetch upstream --tags && git merge upstream/main`

## 约定
- 版本号走**独立下游版本线**：`backend/global/version.go` 的 `VERSION`（当前 `0.1.1`），
  不跟随上游 `0.0.x`
- 镜像：Docker Hub `tianjiangqiji/upstream-ops`（账号 `tianjiangqiji`），
  标签用 **`v` 前缀 + `latest`**（已发布 `v0.1.0`、`v0.1.1`、`latest`）
- 发布命令：`docker build -t tianjiangqiji/upstream-ops:vX.Y.Z -t tianjiangqiji/upstream-ops:latest .`
  然后 `docker push` 两个标签
- 前端构建产物写入 `web/dist`（vite `build.outDir`），供 Go embed 使用
- 已知 fork 身份问题（未修）：
  - README 仍指向上游镜像 `ghcr.io/bejix/upstream-ops` 与 `IMAGE_TAG=v0.0.10`
  - `backend/api/version.go` 硬编码更新检查指向 `bejix/upstream-ops`，
    本 fork 看不到自己仓库的新版本

## 下游定制清单（合并上游时勿丢失）
- `backend/global/version.go` —— 下游版本号
- `docker-compose.yml` —— 自有镜像地址
- `frontend/vite.config.ts` —— `build.outDir` 指向 `../web/dist`
- `backend/connector/newapi/newapi.go` 及 `_test.go` —— 富 `newAPIStatus`
  （`quota_display_type` / `usd_exchange_rate` / `custom_currency_exchange_rate`）、
  `DefaultQuotaPerUnit`、`fetchStatus`、`quotaPerUnit()`
- `frontend/components/gateway/gateway-page.tsx` —— 自动刷新间隔选择 + 刷新去重
- `frontend/components/gateway/models-panel.tsx` —— 本地增补约 131 行
- `frontend/components/monitor/monitor-header.tsx` —— 主题支持「跟随系统」
- `frontend/components/monitor/channel-cards.tsx` —— 阈值/状态条件渲染
- `frontend/components/monitor/channel-subscription-usage-dialog.tsx` —— 无订阅时隐藏
