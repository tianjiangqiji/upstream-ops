package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bejix/upstream-ops/backend/profit"
	"github.com/bejix/upstream-ops/backend/progress"
	"github.com/gin-gonic/gin"
)

// profitService 收益统计服务对外暴露的方法。
type profitService interface {
	Config() (*profit.ConfigView, error)
	SaveConfig(in profit.ConfigInput) (*profit.ConfigView, error)
	TestConfig(ctx context.Context) (*profit.TestResult, error)
	Summary() (*profit.Summary, error)
	Refresh(ctx context.Context) (*profit.Summary, error)
	Retry(ctx context.Context, channelIDs []uint) (*profit.Summary, error)
}

// registerProfit 挂载「今日收益」相关路由。
//
//	GET  /api/profit/summary      顶部卡片与弹窗读取当天最新快照（不触发计算）
//	GET  /api/profit/config       读取主站配置（凭据只回 has_token/has_cookie）
//	PUT  /api/profit/config       保存主站配置（凭据加密落库）
//	POST /api/profit/config/test  用已保存凭据打一次日志统计，验证管理员权限
//	POST /api/profit/refresh      完整更新，SSE 推送每一步进度
//	POST /api/profit/retry        只重试失败供应商，SSE 推送进度
func registerProfit(g *gin.RouterGroup, d *Deps) {
	if d.Profit == nil {
		return
	}
	svc := d.Profit
	pg := g.Group("/profit")
	pg.GET("/summary", func(c *gin.Context) {
		summary, err := svc.Summary()
		if err != nil {
			fail(c, http.StatusInternalServerError, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": summary})
	})
	pg.GET("/config", func(c *gin.Context) {
		view, err := svc.Config()
		if err != nil {
			fail(c, http.StatusInternalServerError, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": view})
	})
	pg.PUT("/config", func(c *gin.Context) {
		var in profit.ConfigInput
		if err := c.ShouldBindJSON(&in); err != nil {
			fail(c, http.StatusBadRequest, err)
			return
		}
		view, err := svc.SaveConfig(in)
		if err != nil {
			fail(c, http.StatusBadRequest, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": view})
	})
	pg.POST("/config/test", func(c *gin.Context) {
		result, err := svc.TestConfig(c.Request.Context())
		if err != nil {
			fail(c, http.StatusBadRequest, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": result})
	})
	pg.POST("/refresh", func(c *gin.Context) {
		runProfitStream(c, func(ctx context.Context) error {
			_, err := svc.Refresh(ctx)
			return err
		})
	})
	pg.POST("/retry", func(c *gin.Context) {
		var in struct {
			ChannelIDs []uint `json:"channel_ids"`
		}
		// 允许空 body：此时按"没有需要重试的供应商"报错，由流里的错误事件展示。
		_ = c.ShouldBindJSON(&in)
		runProfitStream(c, func(ctx context.Context) error {
			_, err := svc.Retry(ctx, in.ChannelIDs)
			return err
		})
	})
}

// runProfitStream 把一轮收益更新包成 SSE 响应。
//
// 进度事件由 profit.Service 内部的 progress.Emit 逐条推出；
// 这里只负责开流与兜底：业务层正常结束会自己发 StageDone / StageError，
// 前置失败（已在跑、未配置主站、凭据不可用）没有业务事件，统一补一条 StageError。
// 前端拿到事件即可，不依赖 HTTP 状态码区分错误类型。
func runProfitStream(c *gin.Context, run func(ctx context.Context) error) {
	obs := setupSSE(c)
	ctx := progress.WithObserver(c.Request.Context(), obs)
	if err := run(ctx); err != nil {
		obs.Emit(progress.Event{Stage: progress.StageError, Message: err.Error(), Time: time.Now()})
	}
}

// profitErrorStatus 把收益统计的前置错误映射成 HTTP 状态码（当前流式接口不使用，供后续同步接口复用）。
func profitErrorStatus(err error) int {
	switch {
	case errors.Is(err, profit.ErrNotConfigured), errors.Is(err, profit.ErrDisabled):
		return http.StatusBadRequest
	case errors.Is(err, profit.ErrAlreadyRunning):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
