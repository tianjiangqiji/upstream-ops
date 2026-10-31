// Package profit 负责「今日收益」的编排与落库。
//
// 收益口径（统一按美元符号展示，round4）：
//
//	总收益   = 主站 GET /api/log/stat      今日消费额度（管理员视角，全站）
//	管理员消费 = 主站 GET /api/log/self/stat 今日消费额度（管理员自己账号的测试消费）
//	成本     = Σ 各供应商渠道今日消费
//	净收益   = 总收益 - 管理员消费 - 成本
//
// 两个日志统计都必须显式传 type=2（消费）：上游在 type=0 时不做类型过滤，
// 会把充值、退款、登录等记录一并计入 sum(quota)，收益会虚高。
//
// 顶部卡片不实时计算（要打主站 + 全部渠道，慢且有失败面），只读当天最新一条快照；
// 用户点「更新」才跑完整流程，失败项沿用渠道上最近一次成功的今日消费并在结果里标注。
package profit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/bejix/upstream-ops/backend/connector"
	"github.com/bejix/upstream-ops/backend/crypto"
	"github.com/bejix/upstream-ops/backend/progress"
	"github.com/bejix/upstream-ops/backend/storage"
)

// ErrAlreadyRunning 已有一轮收益更新在跑，避免并发把主站和全部渠道各打一遍。
var ErrAlreadyRunning = errors.New("收益更新正在进行中，请等待当前流程结束")

// ErrNotConfigured 主站还没配置，无法计算收益。
var ErrNotConfigured = errors.New("尚未配置收益主站，请先在系统设置里填写主站地址与管理员凭据")

// ErrDisabled 收益统计被关闭，不再发起新的更新。
var ErrDisabled = errors.New("收益统计已关闭，请在系统设置里打开「启用收益统计」")

// CostRefresher 只采集今日消费、不碰余额的渠道刷新能力（由 monitor.Service 提供）。
type CostRefresher interface {
	RefreshCosts(ctx context.Context, c *storage.Channel) error
}

// ConnectorConfigurer 把全局的超时 / User-Agent 应用到新建的连接器上。
type ConnectorConfigurer interface {
	ApplyHTTPConfig(conn any)
}

// Service 收益统计服务。
type Service struct {
	settings   *storage.ProfitSettings
	snapshots  *storage.ProfitSnapshots
	channels   *storage.Channels
	refresher  CostRefresher
	configurer ConnectorConfigurer
	cipher     *crypto.Cipher
	log        *slog.Logger

	mu      sync.Mutex
	running bool
}

func NewService(
	settings *storage.ProfitSettings,
	snapshots *storage.ProfitSnapshots,
	channels *storage.Channels,
	refresher CostRefresher,
	configurer ConnectorConfigurer,
	cipher *crypto.Cipher,
	log *slog.Logger,
) *Service {
	return &Service{
		settings:   settings,
		snapshots:  snapshots,
		channels:   channels,
		refresher:  refresher,
		configurer: configurer,
		cipher:     cipher,
		log:        log,
	}
}

// ConfigView 主站配置的对外视图：密文永不出库，只透出"是否已保存凭据"。
type ConfigView struct {
	SiteURL        string `json:"site_url"`
	CredentialMode string `json:"credential_mode"`
	UserID         string `json:"user_id"`
	Enabled        bool   `json:"enabled"`
	HasToken       bool   `json:"has_token"`
	HasCookie      bool   `json:"has_cookie"`
	Configured     bool   `json:"configured"`
}

// ConfigInput 保存主站配置的入参。AccessToken / Cookie 留空表示保留原有凭据。
type ConfigInput struct {
	SiteURL        string `json:"site_url"`
	CredentialMode string `json:"credential_mode"`
	UserID         string `json:"user_id"`
	AccessToken    string `json:"access_token"`
	Cookie         string `json:"cookie"`
	Enabled        *bool  `json:"enabled"`
}

// Config 读取主站配置视图。
func (s *Service) Config() (*ConfigView, error) {
	set, err := s.settings.Get()
	if err != nil {
		return nil, err
	}
	return configView(set), nil
}

// SaveConfig 保存主站配置。凭据走 AES-GCM 加密落库，明文只在这一步存在。
func (s *Service) SaveConfig(in ConfigInput) (*ConfigView, error) {
	cur, err := s.settings.Get()
	if err != nil {
		return nil, err
	}
	siteURL := strings.TrimRight(strings.TrimSpace(in.SiteURL), "/")
	mode := storage.ProfitCredentialMode(strings.TrimSpace(in.CredentialMode))
	if mode == "" {
		mode = storage.ProfitCredentialToken
	}
	if mode != storage.ProfitCredentialToken && mode != storage.ProfitCredentialCookie {
		return nil, fmt.Errorf("未知的凭据模式: %s", mode)
	}
	if siteURL != "" && !strings.HasPrefix(siteURL, "http://") && !strings.HasPrefix(siteURL, "https://") {
		return nil, errors.New("主站地址需要以 http:// 或 https:// 开头")
	}
	if siteURL != "" {
		if token := strings.TrimSpace(in.AccessToken); token != "" {
			enc, err := s.cipher.Encrypt(token)
			if err != nil {
				return nil, fmt.Errorf("加密 access token: %w", err)
			}
			cur.AccessTokenCipher = enc
			cur.CookieCipher = ""
		}
		if cookie := strings.TrimSpace(in.Cookie); cookie != "" {
			enc, err := s.cipher.Encrypt(cookie)
			if err != nil {
				return nil, fmt.Errorf("加密 cookie: %w", err)
			}
			cur.CookieCipher = enc
			cur.AccessTokenCipher = ""
		}
	}

	cur.SiteURL = siteURL
	cur.CredentialMode = mode
	cur.UserID = strings.TrimSpace(in.UserID)
	if in.Enabled != nil {
		cur.Enabled = *in.Enabled
	} else if cur.SiteURL != "" && (cur.AccessTokenCipher != "" || cur.CookieCipher != "") {
		// 首次保存时默认启用，避免用户还要再点一次开关。
		cur.Enabled = true
	}
	if err := s.settings.Save(cur); err != nil {
		return nil, err
	}
	return configView(cur), nil
}

// TestResult 主站连通性测试结果。
type TestResult struct {
	SiteURL  string  `json:"site_url"`
	Revenue  float64 `json:"revenue"`
	Duration float64 `json:"duration_seconds"`
	Message  string  `json:"message"`
}

// TestConfig 用当前保存的凭据打一次日志统计，验证管理员权限可用。
func (s *Service) TestConfig(ctx context.Context) (*TestResult, error) {
	stat, site, err := s.statConnector()
	if err != nil {
		return nil, err
	}
	set, err := s.settings.Get()
	if err != nil {
		return nil, err
	}
	started := time.Now()
	end := time.Now()
	begin := end.Add(-24 * time.Hour)
	res, err := stat.GetLogStat(ctx, site, s.sessionOf(set), connector.LogStatQuery{
		Type:  connector.LogStatTypeConsume,
		Start: begin,
		End:   end,
	})
	if err != nil {
		return nil, fmt.Errorf("读取主站日志统计失败（需要管理员权限）: %w", err)
	}
	return &TestResult{
		SiteURL:  site.SiteURL,
		Revenue:  res.Amount,
		Duration: time.Since(started).Seconds(),
		Message:  fmt.Sprintf("最近 24 小时全站消费 %0.4f，管理员权限可用", res.Amount),
	}, nil
}

// Summary 顶部卡片与弹窗读取的收益结果。
type Summary struct {
	Configured     bool                        `json:"configured"`
	Enabled        bool                        `json:"enabled"`
	SiteURL        string                      `json:"site_url"`
	Day            string                      `json:"day"`
	HasSnapshot    bool                        `json:"has_snapshot"`
	Revenue        float64                     `json:"revenue"`
	AdminCost      float64                     `json:"admin_cost"`
	ChannelCost    float64                     `json:"channel_cost"`
	NetProfit      float64                     `json:"net_profit"`
	RevenueOK      bool                        `json:"revenue_ok"`
	AdminOK        bool                        `json:"admin_ok"`
	ChannelOK      int                         `json:"channel_ok"`
	ChannelFailed  int                         `json:"channel_failed"`
	Channels       []storage.ProfitChannelCost `json:"channels"`
	SampledAt      *time.Time                  `json:"sampled_at,omitempty"`
	Running        bool                        `json:"running"`
	SnapshotFailed int                         `json:"snapshot_failed"`
}

// Summary 读取当天最新一条快照；没有快照时只返回配置状态。
func (s *Service) Summary() (*Summary, error) {
	set, err := s.settings.Get()
	if err != nil {
		return nil, err
	}
	out := &Summary{
		Configured: configured(set),
		Enabled:    set.Enabled,
		SiteURL:    set.SiteURL,
		Day:        dayKey(time.Now()),
		Running:    s.isRunning(),
	}
	snap, err := s.snapshots.LatestForDay(out.Day)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		return out, nil
	}
	out.HasSnapshot = true
	out.Revenue = snap.Revenue
	out.AdminCost = snap.AdminCost
	out.ChannelCost = snap.ChannelCost
	out.NetProfit = snap.NetProfit
	out.RevenueOK = snap.RevenueOK
	out.AdminOK = snap.AdminOK
	out.ChannelOK = snap.ChannelOK
	out.ChannelFailed = snap.ChannelFailed
	out.Channels = storage.DecodeProfitChannels(snap.ChannelsJSON)
	sampledAt := snap.SampledAt
	out.SampledAt = &sampledAt
	out.SnapshotFailed = snap.ChannelFailed
	return out, nil
}

// Refresh 跑一轮完整收益更新：主站总收益 → 管理员消费 → 逐供应商今日消费。
func (s *Service) Refresh(ctx context.Context) (*Summary, error) {
	return s.run(ctx, nil)
}

// Retry 只重试指定渠道，主站的两个金额沿用上一轮快照，未重试的渠道沿用其上次成功金额。
func (s *Service) Retry(ctx context.Context, channelIDs []uint) (*Summary, error) {
	if len(channelIDs) == 0 {
		return nil, errors.New("没有需要重试的供应商")
	}
	return s.run(ctx, channelIDs)
}

// run 收益更新的唯一执行路径。retryIDs 为 nil 表示完整更新。
func (s *Service) run(ctx context.Context, retryIDs []uint) (*Summary, error) {
	if !s.begin() {
		return nil, ErrAlreadyRunning
	}
	defer s.end()

	set, err := s.settings.Get()
	if err != nil {
		return nil, err
	}
	if !configured(set) {
		return nil, ErrNotConfigured
	}
	if !set.Enabled {
		return nil, ErrDisabled
	}

	now := time.Now()
	day := dayKey(now)
	prev, err := s.snapshots.LatestForDay(day)
	if err != nil {
		return nil, err
	}
	// 主站还没配置凭据时不必走到这里（上面已拦），但凭据可能刚失效：
	// 重试模式下直接用上一轮的金额，避免一次渠道重试又把主站打挂。
	reuseSite := retryIDs != nil && prev != nil

	var revenue, adminCost float64
	var revenueOK, adminOK bool

	if reuseSite {
		revenue, adminCost = prev.Revenue, prev.AdminCost
		revenueOK, adminOK = prev.RevenueOK, prev.AdminOK
		progress.OK(ctx, progress.StageRevenue, fmt.Sprintf("沿用本轮总收益 %0.4f", revenue))
		progress.OK(ctx, progress.StageAdminCost, fmt.Sprintf("沿用本轮管理员消费 %0.4f", adminCost))
	} else {
		stat, site, err := s.statConnector()
		if err != nil {
			return nil, err
		}
		session := s.sessionOf(set)

		progress.Start(ctx, progress.StageRevenue, "拉取主站今日总收益…")
		res, err := stat.GetLogStat(ctx, site, session, todayQuery(now))
		if err != nil {
			progress.Fail(ctx, progress.StageRevenue, fmt.Sprintf("总收益获取失败：%v", err))
			if prev != nil {
				revenue, revenueOK = prev.Revenue, false
			}
		} else {
			revenue, revenueOK = res.Amount, true
			progress.OK(ctx, progress.StageRevenue, fmt.Sprintf("今日总收益 %0.4f", revenue), map[string]any{"revenue": revenue})
		}

		progress.Start(ctx, progress.StageAdminCost, "拉取管理员测试消费…")
		self, err := stat.GetSelfLogStat(ctx, site, session, todayQuery(now))
		if err != nil {
			progress.Fail(ctx, progress.StageAdminCost, fmt.Sprintf("管理员消费获取失败：%v", err))
			if prev != nil {
				adminCost, adminOK = prev.AdminCost, false
			}
		} else {
			adminCost, adminOK = self.Amount, true
			progress.OK(ctx, progress.StageAdminCost, fmt.Sprintf("管理员消费 %0.4f", adminCost), map[string]any{"admin_cost": adminCost})
		}
	}

	list, err := s.channels.List()
	if err != nil {
		return nil, err
	}
	retrySet := make(map[uint]bool, len(retryIDs))
	for _, id := range retryIDs {
		retrySet[id] = true
	}

	attempts := len(list)
	if retryIDs != nil {
		attempts = 0
		for i := range list {
			if retrySet[list[i].ID] {
				attempts++
			}
		}
	}
	if attempts == 0 {
		progress.OK(ctx, progress.StageCost, "没有需要重试的供应商，沿用上一轮金额")
	} else {
		progress.Start(ctx, progress.StageCost, fmt.Sprintf("重新采集 %d 个供应商的今日消费…", attempts))
	}
	base := progress.FromContext(ctx)
	details := make([]storage.ProfitChannelCost, 0, len(list))
	channelOK, channelFailed := 0, 0
	total := len(list)

	// 重试模式下没被选中的渠道不参与本轮采集：金额和来源都直接继承上一轮快照，
	// 不能重新用 carriedCost 判定（那会把上一轮已经成功的渠道误标成 fallback/missing，
	// 前端就会把一堆本来正常的渠道列进「需要重试」）。
	var prevDetails map[uint]storage.ProfitChannelCost
	if retryIDs != nil && prev != nil {
		for _, d := range storage.DecodeProfitChannels(prev.ChannelsJSON) {
			if prevDetails == nil {
				prevDetails = make(map[uint]storage.ProfitChannelCost, len(list))
			}
			prevDetails[d.ChannelID] = d
		}
	}

	for i := range list {
		ch := &list[i]
		attempt := retryIDs == nil || retrySet[ch.ID]
		scoped := profitScopedObserver{base: base, channelID: ch.ID, channelName: ch.Name, index: i + 1, total: total}
		cctx := progress.WithObserver(ctx, scoped)

		if !attempt {
			detail := storage.ProfitChannelCost{ChannelID: ch.ID, ChannelName: ch.Name, Source: sourceCarried}
			if p, ok := prevDetails[ch.ID]; ok {
				detail.Cost = p.Cost
				// 上一轮就没拿到真实金额的渠道，本轮没被重试，失败状态必须保留，
				// 否则界面会把「还没修好」的供应商当成正常。
				if isFailedSource(p.Source) {
					detail.Source = p.Source
					channelFailed++
				}
			} else if cost, source := carriedCost(ch, now); source != sourceMissing {
				// 上一轮快照里没有这个渠道（刚新建的），退回到渠道上的今日消费。
				detail.Cost, detail.Source = cost, source
			}
			details = append(details, detail)
			continue
		}

		if err := s.refresher.RefreshCosts(cctx, ch); err != nil {
			cost, source := carriedCost(ch, now)
			details = append(details, storage.ProfitChannelCost{
				ChannelID:   ch.ID,
				ChannelName: ch.Name,
				Cost:        cost,
				Source:      source,
			})
			channelFailed++
			continue
		}
		// 采集成功后渠道上的 today_cost 已被写入，重新读一遍拿到本轮真实值。
		fresh, err := s.channels.FindByID(ch.ID)
		if err != nil || fresh == nil {
			fresh = ch
		}
		cost := 0.0
		if v := fresh.EffectiveTodayCost(now); v != nil {
			cost = *v
		}
		details = append(details, storage.ProfitChannelCost{
			ChannelID:   ch.ID,
			ChannelName: ch.Name,
			Cost:        cost,
			Source:      sourceFresh,
		})
		channelOK++
	}

	channelCost := 0.0
	for _, d := range details {
		channelCost += d.Cost
	}
	channelCost = round4(channelCost)
	revenue = round4(revenue)
	adminCost = round4(adminCost)
	net := round4(revenue - adminCost - channelCost)

	// 收口「成本」这一步：每个渠道的消费事件带 channel_id，这里用不带 channel_id
	// 的同名事件把汇总行标记为结束，避免前端进度列表在流程结束后仍留着转圈。
	progress.OK(ctx, progress.StageCost, fmt.Sprintf("成本合计 %0.4f", channelCost), map[string]any{"channel_cost": channelCost})

	snap := &storage.ProfitSnapshot{
		Day:           day,
		Revenue:       revenue,
		AdminCost:     adminCost,
		ChannelCost:   channelCost,
		NetProfit:     net,
		RevenueOK:     revenueOK,
		AdminOK:       adminOK,
		ChannelOK:     channelOK,
		ChannelFailed: channelFailed,
		ChannelsJSON:  storage.EncodeProfitChannels(details),
		SampledAt:     time.Now(),
	}
	if err := s.snapshots.Create(snap); err != nil {
		return nil, fmt.Errorf("保存收益快照: %w", err)
	}

	summary := &Summary{
		Configured:     true,
		Enabled:        set.Enabled,
		SiteURL:        set.SiteURL,
		Day:            day,
		HasSnapshot:    true,
		Revenue:        revenue,
		AdminCost:      adminCost,
		ChannelCost:    channelCost,
		NetProfit:      net,
		RevenueOK:      revenueOK,
		AdminOK:        adminOK,
		ChannelOK:      channelOK,
		ChannelFailed:  channelFailed,
		Channels:       details,
		SampledAt:      &snap.SampledAt,
		SnapshotFailed: channelFailed,
	}

	msg := fmt.Sprintf("计算完成：总收益 %0.4f - 管理员消费 %0.4f - 成本 %0.4f = 净收益 %0.4f", revenue, adminCost, channelCost, net)
	payload := map[string]any{"summary": summary}
	if channelFailed > 0 {
		progress.FailData(ctx, progress.StageError, fmt.Sprintf("%s（%d 个供应商采集失败，已按上次成功金额计入）", msg, channelFailed), payload)
	} else if !revenueOK || !adminOK {
		progress.FailData(ctx, progress.StageError, fmt.Sprintf("%s（主站数据获取不完整）", msg), payload)
	} else {
		progress.OK(ctx, progress.StageDone, msg, payload)
	}
	return summary, nil
}

const (
	sourceFresh    = "fresh"
	sourceFallback = "fallback"
	sourceMissing  = "missing"
	sourceCarried  = "carried"
)

// isFailedSource 判断某笔明细是否属于「本轮没拿到真实金额」。
// carried 不算：它表示本轮压根没采集这个渠道（重试时只跑选中的几个），
// 金额直接沿用上一轮快照，上一轮是什么结论就还是什么结论。
func isFailedSource(source string) bool {
	return source == sourceFallback || source == sourceMissing
}

// carriedCost 采集失败时沿用的金额：有当天记录就用它，跨天或从未采过则按 0。
func carriedCost(ch *storage.Channel, now time.Time) (float64, string) {
	if v := ch.EffectiveTodayCost(now); v != nil {
		return *v, sourceFallback
	}
	return 0, sourceMissing
}

// statConnector 构造主站连接器。收益统计只支持 NewAPI 系（/api/log/stat 是它的管理接口）。
func (s *Service) statConnector() (connector.StatConnector, *connector.Channel, error) {
	set, err := s.settings.Get()
	if err != nil {
		return nil, nil, err
	}
	if !configured(set) {
		return nil, nil, ErrNotConfigured
	}
	conn, err := connector.For(connector.TypeNewAPI)
	if err != nil {
		return nil, nil, err
	}
	stat, ok := conn.(connector.StatConnector)
	if !ok {
		return nil, nil, errors.New("当前连接器不支持日志统计接口")
	}
	if s.configurer != nil {
		s.configurer.ApplyHTTPConfig(conn)
	}
	site := &connector.Channel{
		Type:     connector.TypeNewAPI,
		SiteURL:  strings.TrimRight(set.SiteURL, "/"),
		Username: set.UserID,
	}
	return stat, site, nil
}

// sessionOf 把加密保存的凭据还原成连接器会话。
func (s *Service) sessionOf(set *storage.ProfitSetting) *connector.AuthSession {
	session := &connector.AuthSession{UserID: set.UserID}
	if set.AccessTokenCipher != "" {
		if token, err := s.cipher.Decrypt(set.AccessTokenCipher); err == nil {
			session.AccessToken = token
		} else if s.log != nil {
			s.log.Warn("解密主站 access token 失败", "err", err)
		}
	}
	if set.CookieCipher != "" {
		if cookie, err := s.cipher.Decrypt(set.CookieCipher); err == nil {
			session.Cookie = cookie
		} else if s.log != nil {
			s.log.Warn("解密主站 cookie 失败", "err", err)
		}
	}
	return session
}

func configView(set *storage.ProfitSetting) *ConfigView {
	mode := string(set.CredentialMode)
	if mode == "" {
		mode = string(storage.ProfitCredentialToken)
	}
	return &ConfigView{
		SiteURL:        set.SiteURL,
		CredentialMode: mode,
		UserID:         set.UserID,
		Enabled:        set.Enabled,
		HasToken:       set.AccessTokenCipher != "",
		HasCookie:      set.CookieCipher != "",
		Configured:     configured(set),
	}
}

// configured 主站地址与任一凭据齐备才算配置完成。
func configured(set *storage.ProfitSetting) bool {
	if set == nil || strings.TrimSpace(set.SiteURL) == "" {
		return false
	}
	return set.AccessTokenCipher != "" || set.CookieCipher != ""
}

func (s *Service) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *Service) end() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

func (s *Service) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// profitScopedObserver 给渠道级事件补上渠道信息，弹窗据此按供应商分行显示。
type profitScopedObserver struct {
	base        progress.Observer
	channelID   uint
	channelName string
	index       int
	total       int
}

func (o profitScopedObserver) Emit(ev progress.Event) {
	ev.ChannelID = o.channelID
	ev.ChannelName = o.channelName
	ev.Index = o.index
	ev.Total = o.total
	o.base.Emit(ev)
}

// todayQuery 今日 0 点（Asia/Shanghai）到当前时刻的日志统计区间。
func todayQuery(now time.Time) connector.LogStatQuery {
	return connector.LogStatQuery{
		Type:  connector.LogStatTypeConsume,
		Start: dayStart(now),
		End:   now,
	}
}

var profitLocation = loadProfitLocation()

func loadProfitLocation() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("Asia/Shanghai", 8*60*60)
}

func dayStart(t time.Time) time.Time {
	local := t.In(profitLocation)
	y, m, d := local.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, profitLocation)
}

func dayKey(t time.Time) string {
	return t.In(profitLocation).Format("2006-01-02")
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}
