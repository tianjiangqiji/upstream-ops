// Package aiapi 实现 cn.aiapi.bot（AIApi 自研平台）的 connector。
//
// 该站点既不是 NewAPI 也不是 Sub2API：登录态由 site-session Cookie 承载，
// 接口统一返回 {ok, data} / {ok:false, message, errorCode}。
package aiapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bejix/upstream-ops/backend/connector"
	"github.com/go-resty/resty/v2"
)

func init() {
	connector.RegisterPlugin(connector.Plugin{
		Descriptor: connector.PluginDescriptor{
			Type:        connector.TypeAIApi,
			Name:        "AIApi",
			Description: "AIApi 自研平台（cn.aiapi.bot）：余额 / 消费 / 分组倍率 / 公告",
			Capabilities: []connector.Capability{
				connector.CapLogin,
				connector.CapTokenCredential,
				connector.CapBalance,
				connector.CapCosts,
				connector.CapRates,
				connector.CapAnnouncements,
			},
			TokenFields: []connector.TokenField{
				{
					Key:         "cookie",
					Label:       "站点 Cookie",
					Placeholder: "site-session=sts_...",
					Help:        "浏览器登录 cn.aiapi.bot 后，从 DevTools 复制 site-session Cookie（约 7 天有效）",
					Required:    true,
					Secret:      true,
					Target:      "cookie",
				},
			},
			TokenHint: "从浏览器复制 site-session Cookie",
		},
		Factory: func() connector.Connector { return New() },
	})
}

// Client AIApi connector 实现。
type Client struct {
	http *resty.Client
}

func New() *Client {
	c := resty.New().
		SetTimeout(30*time.Second).
		SetHeader("User-Agent", "upstream-ops/0.1").
		SetHeader("Accept", "application/json")
	return &Client{http: c}
}

func (c *Client) SetProxy(proxyURL string) {
	if strings.TrimSpace(proxyURL) == "" {
		return
	}
	c.http.SetProxy(proxyURL)
}

func (c *Client) SetHTTPConfig(cfg connector.HTTPConfig) {
	if cfg.Timeout > 0 {
		c.http.SetTimeout(cfg.Timeout)
	}
	if strings.TrimSpace(cfg.UserAgent) != "" {
		c.http.SetHeader("User-Agent", cfg.UserAgent)
	}
}

// envelope AIApi 统一响应外壳。
type envelope struct {
	OK        bool            `json:"ok"`
	Data      json.RawMessage `json:"data"`
	Message   string          `json:"message"`
	ErrorCode string          `json:"errorCode"`
	Service   string          `json:"service"`
}

// do 发起请求并解包统一响应；未登录 / 业务失败统一返回带 message 的错误。
func (c *Client) do(
	ctx context.Context,
	method string,
	ch *connector.Channel,
	session *connector.AuthSession,
	path string,
	query url.Values,
	body any,
) (json.RawMessage, error) {
	raw, err := c.exchange(ctx, method, ch, session, path, query, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("aiapi 响应解析失败: %w", err)
	}
	return env.Data, nil
}

// exchange 发起请求、校验统一响应并返回原始响应体。
// 用于 /api/auth/session 这类把业务字段放在顶层的接口（没有 data 外壳）。
func (c *Client) exchange(
	ctx context.Context,
	method string,
	ch *connector.Channel,
	session *connector.AuthSession,
	path string,
	query url.Values,
	body any,
) ([]byte, error) {
	site := strings.TrimRight(strings.TrimSpace(ch.SiteURL), "/")
	if site == "" {
		return nil, errors.New("aiapi 未配置站点地址")
	}
	req := c.http.R().SetContext(ctx)
	applySession(req, session)
	if query != nil {
		req.SetQueryParamsFromValues(query)
	}
	if body != nil {
		req.SetHeader("Content-Type", "application/json").SetBody(body)
	}
	resp, err := req.Execute(method, site+path)
	if err != nil {
		return nil, fmt.Errorf("aiapi 请求失败: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(resp.Body(), &env); err != nil {
		if resp.StatusCode() >= 400 {
			return nil, connector.HTTPStatusError(resp.StatusCode(), resp.Body())
		}
		return nil, fmt.Errorf("aiapi 响应解析失败: %w", err)
	}
	if !env.OK {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode())
		}
		if code := strings.TrimSpace(env.ErrorCode); code != "" {
			msg = fmt.Sprintf("%s (%s)", msg, code)
		}
		return nil, fmt.Errorf("aiapi: %s", msg)
	}
	return resp.Body(), nil
}

func (c *Client) get(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, path string, query url.Values) (json.RawMessage, error) {
	return c.do(ctx, "GET", ch, session, path, query, nil)
}

func applySession(req *resty.Request, session *connector.AuthSession) {
	if session == nil {
		return
	}
	if cookie := strings.TrimSpace(session.Cookie); cookie != "" {
		// 允许用户只填 site-session 的值，而不是整段 "site-session=..."。
		if !strings.Contains(cookie, "=") {
			cookie = "site-session=" + cookie
		}
		req.SetHeader("Cookie", cookie)
	}
	if token := strings.TrimSpace(session.AccessToken); token != "" {
		req.SetHeader("Authorization", "Bearer "+token)
	}
}

func (c *Client) GetTurnstileSiteKey(ctx context.Context, ch *connector.Channel) (string, error) {
	// 登录接口不需要 Turnstile；站点仅在注册时使用。声明为空避免上层触发无谓打码。
	return "", nil
}

func (c *Client) Login(ctx context.Context, ch *connector.Channel) (*connector.AuthSession, error) {
	site := strings.TrimRight(strings.TrimSpace(ch.SiteURL), "/")
	if site == "" {
		return nil, errors.New("aiapi 未配置站点地址")
	}
	payload := map[string]any{
		"identifier": strings.TrimSpace(ch.Username),
		"password":   ch.Password,
	}
	for k, v := range ch.LoginExtraParams {
		payload[k] = v
	}
	resp, err := c.http.R().
		SetContext(ctx).
		SetHeader("Content-Type", "application/json").
		SetBody(payload).
		Post(site + "/api/auth/login")
	if err != nil {
		return nil, fmt.Errorf("aiapi 登录请求失败: %w", err)
	}
	var env struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
		User    struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(resp.Body(), &env); err != nil {
		return nil, connector.HTTPStatusError(resp.StatusCode(), resp.Body())
	}
	if !env.OK {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = "邮箱或密码错误"
		}
		return nil, fmt.Errorf("aiapi 登录失败: %s", msg)
	}

	session := &connector.AuthSession{
		UserID:    strconv.FormatInt(env.User.ID, 10),
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name != "site-session" {
			continue
		}
		session.Cookie = cookie.Name + "=" + cookie.Value
		if !cookie.Expires.IsZero() {
			session.ExpiresAt = cookie.Expires
		}
	}
	if session.Cookie == "" {
		return nil, errors.New("aiapi 登录失败: 响应中没有 site-session Cookie")
	}
	return session, nil
}

func (c *Client) CheckAuth(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) error {
	// /api/auth/session 的业务字段在顶层（没有 data 外壳），因此用 exchange 拿原始响应体。
	raw, err := c.exchange(ctx, "GET", ch, session, "/api/auth/session", nil, nil)
	if err != nil {
		return err
	}
	var payload struct {
		User *struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("aiapi 会话解析失败: %w", err)
	}
	if payload.User == nil {
		return errors.New("aiapi 会话无效")
	}
	return nil
}

func (c *Client) GetBalance(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) (*connector.BalanceResult, error) {
	data, err := c.get(ctx, ch, session, "/api/me/commerce/get-user-asset-summary", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		AssetReadModel struct {
			WalletAvailableUsd *float64 `json:"walletAvailableUsd"`
			WalletBalanceUsd   *float64 `json:"walletBalanceUsd"`
		} `json:"assetReadModel"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("aiapi 资产解析失败: %w", err)
	}
	model := payload.AssetReadModel
	var balance float64
	switch {
	case model.WalletAvailableUsd != nil:
		balance = *model.WalletAvailableUsd
	case model.WalletBalanceUsd != nil:
		balance = *model.WalletBalanceUsd
	default:
		return nil, errors.New("aiapi 资产响应缺少钱包余额字段")
	}
	return &connector.BalanceResult{Balance: balance, SampledAt: time.Now()}, nil
}

func (c *Client) GetCosts(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) (*connector.CostResult, error) {
	today, err := c.todayCost(ctx, ch, session)
	if err != nil {
		return nil, err
	}
	total, err := c.totalCost(ctx, ch, session)
	if err != nil {
		return nil, err
	}
	return &connector.CostResult{TodayCost: today, TotalCost: total}, nil
}

// todayCost 用 usage/history/summary 拉取北京时间当天 0 点到次日 0 点的消费。
func (c *Client) todayCost(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) (float64, error) {
	loc := shanghaiLocation()
	now := time.Now().In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	query := url.Values{}
	query.Set("startAt", start.Format(time.RFC3339))
	query.Set("endAt", start.AddDate(0, 0, 1).Format(time.RFC3339))
	data, err := c.get(ctx, ch, session, "/api/me/usage/history/summary", query)
	if err != nil {
		return 0, err
	}
	var payload struct {
		CostUsd float64 `json:"costUsd"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("aiapi 今日消费解析失败: %w", err)
	}
	return roundMoney(payload.CostUsd), nil
}

// totalCost 用活动日历（365 天）求和近似累计消费：站点没有直接提供 lifetime 字段。
func (c *Client) totalCost(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) (float64, error) {
	query := url.Values{}
	query.Set("range", "365d")
	data, err := c.get(ctx, ch, session, "/api/me/usage/activity-calendar", query)
	if err != nil {
		return 0, err
	}
	var payload struct {
		Days []struct {
			CostUsd float64 `json:"costUsd"`
		} `json:"days"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return 0, fmt.Errorf("aiapi 累计消费解析失败: %w", err)
	}
	var total float64
	for _, day := range payload.Days {
		total += day.CostUsd
	}
	return roundMoney(total), nil
}

func (c *Client) GetRates(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) ([]connector.RateResult, error) {
	data, err := c.get(ctx, ch, session, "/api/me/routing-groups", nil)
	if err != nil {
		return nil, err
	}
	var groups []struct {
		ID                int64   `json:"id"`
		Name              string  `json:"name"`
		Description       string  `json:"description"`
		BillingMultiplier float64 `json:"billingMultiplier"`
		Status            string  `json:"status"`
	}
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, fmt.Errorf("aiapi 分组解析失败: %w", err)
	}
	out := make([]connector.RateResult, 0, len(groups))
	for _, g := range groups {
		if g.Status != "" && g.Status != "active" {
			continue
		}
		if strings.TrimSpace(g.Name) == "" {
			continue
		}
		groupID := g.ID
		out = append(out, connector.RateResult{
			GroupID:     &groupID,
			ModelName:   strings.TrimSpace(g.Name),
			Description: strings.TrimSpace(g.Description),
			Ratio:       g.BillingMultiplier,
		})
	}
	return out, nil
}

func (c *Client) GetAnnouncements(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) ([]connector.AnnouncementResult, error) {
	data, err := c.get(ctx, ch, session, "/api/me/announcements", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Announcements []struct {
			ID        int64      `json:"id"`
			Title     string     `json:"title"`
			Body      string     `json:"body"`
			Severity  string     `json:"severity"`
			ActionHref *string   `json:"actionHref"`
			Status    string     `json:"status"`
			PublishedAt *time.Time `json:"publishedAt"`
			UpdatedAt *time.Time `json:"updatedAt"`
		} `json:"announcements"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("aiapi 公告解析失败: %w", err)
	}
	out := make([]connector.AnnouncementResult, 0, len(payload.Announcements))
	for _, a := range payload.Announcements {
		if a.Status != "" && a.Status != "published" {
			continue
		}
		link := ""
		if a.ActionHref != nil {
			link = strings.TrimSpace(*a.ActionHref)
		}
		sourceUpdated := a.PublishedAt
		if a.UpdatedAt != nil {
			sourceUpdated = a.UpdatedAt
		}
		out = append(out, connector.AnnouncementResult{
			SourceKey:       strconv.FormatInt(a.ID, 10),
			Title:           strings.TrimSpace(a.Title),
			Content:         a.Body,
			Type:            strings.TrimSpace(a.Severity),
			Link:            link,
			PublishedAt:     a.PublishedAt,
			SourceUpdatedAt: sourceUpdated,
		})
	}
	return out, nil
}

func (c *Client) RedeemCode(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, code string) (*connector.RedeemResult, error) {
	return nil, connector.Unsupported("兑换码")
}

func (c *Client) GetRechargeInfo(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) (*connector.RechargeInfo, error) {
	return nil, connector.Unsupported("充值")
}

func (c *Client) CreateRecharge(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, req connector.RechargeRequest) (*connector.RechargeLaunch, error) {
	return nil, connector.Unsupported("充值")
}

func (c *Client) GetSubscriptionInfo(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) (*connector.SubscriptionInfo, error) {
	return nil, connector.Unsupported("订阅")
}

func (c *Client) CreateSubscription(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, req connector.SubscriptionRequest) (*connector.SubscriptionLaunch, error) {
	return nil, connector.Unsupported("订阅")
}

func (c *Client) GetSubscriptionUsage(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) (*connector.SubscriptionUsageInfo, error) {
	return nil, connector.Unsupported("订阅用量")
}

func (c *Client) ListAPIKeys(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, query connector.APIKeyQuery) (*connector.APIKeyPage, error) {
	return nil, connector.Unsupported("API Key 管理")
}

func (c *Client) ListAPIKeyGroups(ctx context.Context, ch *connector.Channel, session *connector.AuthSession) ([]connector.APIKeyGroup, error) {
	return nil, connector.Unsupported("API Key 管理")
}

func (c *Client) CreateAPIKey(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, req connector.APIKeyCreateRequest) (*connector.APIKey, error) {
	return nil, connector.Unsupported("API Key 管理")
}

func (c *Client) UpdateAPIKey(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, id int64, req connector.APIKeyUpdateRequest) (*connector.APIKey, error) {
	return nil, connector.Unsupported("API Key 管理")
}

func (c *Client) DeleteAPIKey(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, id int64) error {
	return connector.Unsupported("API Key 管理")
}

func (c *Client) RevealAPIKey(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, id int64) (string, error) {
	return "", connector.Unsupported("API Key 管理")
}

func shanghaiLocation() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*60*60)
}

func roundMoney(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}
