// Package profit 负责「今日收益」的编排与落库。
//
// 收益口径（统一按美元符号展示，round4）：
//
//	总收益   = Σ 各启用自有站点 GET /api/log/stat        今日消费额度（管理员视角，全站）
//	管理员消费 = Σ 各启用自有站点 GET /api/log/self/stat 今日消费额度（管理员自己账号的测试消费）
//	成本     = Σ 各供应商渠道今日消费
//	净收益   = 总收益 - 管理员消费 - 成本
//
// 单个站点的收益 = 该站总收益 - 该站管理员消费；渠道成本是全局的，只从合计里减一次。
// 站点之间互不影响：某个站点拉取失败只把该站的金额标成不完整并沿用上一轮，
// 其它站点与渠道照常计算。
//
// 两个日志统计都必须显式传 type=2（消费）：上游在 type=0 时不做类型过滤，
// 会把充值、退款、登录等记录一并计入 sum(quota)，收益会虚高。
//
// 顶部卡片不实时计算（要打全部站点 + 全部渠道，慢且有失败面），只读当天最新一条快照；
// 用户点「更新」才跑完整流程，失败项沿用最近一次成功的金额并在结果里标注。
package profit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bejix/upstream-ops/backend/connector"
	"github.com/bejix/upstream-ops/backend/crypto"
	"github.com/bejix/upstream-ops/backend/progress"
	"github.com/bejix/upstream-ops/backend/storage"
	"gorm.io/gorm"
)

// ErrAlreadyRunning 已有一轮收益更新在跑，避免并发把全部站点和渠道各打一遍。
var ErrAlreadyRunning = errors.New("收益更新正在进行中，请等待当前流程结束")

// ErrNotConfigured 没有任何可用的自有站点，无法计算收益。
var ErrNotConfigured = errors.New("尚未配置收益站点，请先在系统设置里添加一个自有站点并填写凭据")

// ErrDisabled 收益统计被关闭，不再发起新的更新。
var ErrDisabled = errors.New("收益统计已关闭，请在系统设置里打开「启用收益统计」")

// ErrSiteNotFound 站点不存在（可能已被删除）。
var ErrSiteNotFound = errors.New("站点不存在")

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
	sites      *storage.ProfitSites
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
	sites *storage.ProfitSites,
	snapshots *storage.ProfitSnapshots,
	channels *storage.Channels,
	refresher CostRefresher,
	configurer ConnectorConfigurer,
	cipher *crypto.Cipher,
	log *slog.Logger,
) *Service {
	return &Service{
		settings:   settings,
		sites:      sites,
		snapshots:  snapshots,
		channels:   channels,
		refresher:  refresher,
		configurer: configurer,
		cipher:     cipher,
		log:        log,
	}
}

// SiteView 单个自有站点的对外视图：密文永不出库，只透出「是否已保存凭据」。
type SiteView struct {
	ID             uint   `json:"id"`
	Name           string `json:"name"`
	SiteURL        string `json:"site_url"`
	CredentialMode string `json:"credential_mode"`
	UserID         string `json:"user_id"`
	Enabled        bool   `json:"enabled"`
	SortOrder      int    `json:"sort_order"`
	HasToken       bool   `json:"has_token"`
	HasCookie      bool   `json:"has_cookie"`
	Configured     bool   `json:"configured"`
}

// ConfigView 收益统计的全局配置视图。
type ConfigView struct {
	// Enabled 收益统计总开关（与站点数量无关）。
	Enabled bool `json:"enabled"`
	// Configured 至少有一个启用且凭据齐备的站点，才能点「更新」。
	Configured bool `json:"configured"`
	// Sites 全部站点（含停用），按展示顺序。
	Sites []SiteView `json:"sites"`
}

// ConfigInput 全局开关的入参。站点配置走 SiteInput。
type ConfigInput struct {
	Enabled *bool `json:"enabled"`
}

// SiteInput 新建 / 修改站点的入参。AccessToken / Cookie 留空表示保留原有凭据。
type SiteInput struct {
	Name           string `json:"name"`
	SiteURL        string `json:"site_url"`
	CredentialMode string `json:"credential_mode"`
	UserID         string `json:"user_id"`
	AccessToken    string `json:"access_token"`
	Cookie         string `json:"cookie"`
	Enabled        *bool  `json:"enabled"`
	SortOrder      *int   `json:"sort_order"`
}

// Config 读取全局开关与全部站点。
func (s *Service) Config() (*ConfigView, error) {
	set, err := s.settings.Get()
	if err != nil {
		return nil, err
	}
	sites, err := s.sites.List()
	if err != nil {
		return nil, err
	}
	return configView(set, sites), nil
}

// SaveConfig 只保存全局开关；站点由 CreateSite / UpdateSite 维护。
func (s *Service) SaveConfig(in ConfigInput) (*ConfigView, error) {
	cur, err := s.settings.Get()
	if err != nil {
		return nil, err
	}
	if in.Enabled != nil {
		cur.Enabled = *in.Enabled
	}
	if err := s.settings.Save(cur); err != nil {
		return nil, err
	}
	sites, err := s.sites.List()
	if err != nil {
		return nil, err
	}
	return configView(cur, sites), nil
}

// CreateSite 新增一个自有站点。首次保存默认启用（凭据齐备时才会参与计算）。
func (s *Service) CreateSite(in SiteInput) (*SiteView, error) {
	site := &storage.ProfitSite{}
	if err := s.applySiteInput(site, in, true); err != nil {
		return nil, err
	}
	if err := s.sites.Create(site); err != nil {
		return nil, fmt.Errorf("保存站点: %w", err)
	}
	// 首次添加一个可用站点时自动打开总开关，省掉用户再点一次「启用收益统计」。
	if set, err := s.settings.Get(); err == nil && !set.Enabled && site.Enabled && siteConfigured(site) {
		set.Enabled = true
		if err := s.settings.Save(set); err != nil && s.log != nil {
			s.log.Warn("自动开启收益统计失败", "err", err)
		}
	}
	return siteView(site), nil
}

// UpdateSite 修改站点；in 里留空的凭据字段保留原值。
func (s *Service) UpdateSite(id uint, in SiteInput) (*SiteView, error) {
	site, err := s.sites.FindByID(id)
	if err != nil {
		return nil, notFoundOr(err, "读取站点")
	}
	if err := s.applySiteInput(site, in, false); err != nil {
		return nil, err
	}
	if err := s.sites.Update(site); err != nil {
		return nil, fmt.Errorf("保存站点: %w", err)
	}
	return siteView(site), nil
}

// DeleteSite 删除站点。历史快照里的金额不会被改写，需要重新更新才与当前配置一致。
func (s *Service) DeleteSite(id uint) error {
	if _, err := s.sites.FindByID(id); err != nil {
		return notFoundOr(err, "读取站点")
	}
	if err := s.sites.Delete(id); err != nil {
		return fmt.Errorf("删除站点: %w", err)
	}
	return nil
}

// applySiteInput 校验并把入参写进实体。creating 区分新建与修改的默认值。
func (s *Service) applySiteInput(site *storage.ProfitSite, in SiteInput, creating bool) error {
	siteURL := strings.TrimRight(strings.TrimSpace(in.SiteURL), "/")
	if siteURL == "" {
		return errors.New("站点地址不能为空")
	}
	if !strings.HasPrefix(siteURL, "http://") && !strings.HasPrefix(siteURL, "https://") {
		return errors.New("站点地址需要以 http:// 或 https:// 开头")
	}
	mode := storage.ProfitCredentialMode(strings.TrimSpace(in.CredentialMode))
	if mode == "" {
		mode = storage.ProfitCredentialToken
	}
	if mode != storage.ProfitCredentialToken && mode != storage.ProfitCredentialCookie {
		return fmt.Errorf("未知的凭据模式: %s", mode)
	}
	if token := strings.TrimSpace(in.AccessToken); token != "" {
		enc, err := s.cipher.Encrypt(token)
		if err != nil {
			return fmt.Errorf("加密 access token: %w", err)
		}
		site.AccessTokenCipher = enc
		site.CookieCipher = ""
	}
	if cookie := strings.TrimSpace(in.Cookie); cookie != "" {
		enc, err := s.cipher.Encrypt(cookie)
		if err != nil {
			return fmt.Errorf("加密 cookie: %w", err)
		}
		site.CookieCipher = enc
		site.AccessTokenCipher = ""
	}

	site.SiteURL = siteURL
	site.CredentialMode = mode
	site.UserID = strings.TrimSpace(in.UserID)

	if name := strings.TrimSpace(in.Name); name != "" {
		site.Name = truncateRunes(name, 64)
	} else if creating || strings.TrimSpace(site.Name) == "" {
		site.Name = fallbackSiteName(siteURL)
	}
	if in.SortOrder != nil {
		site.SortOrder = *in.SortOrder
	}
	if in.Enabled != nil {
		site.Enabled = *in.Enabled
	} else if creating {
		// 新建默认启用：凭据齐备才会进入计算，未齐备的站点只是「待配置」。
		site.Enabled = true
	}
	return nil
}

// TestResult 单个站点的连通性测试结果。
type TestResult struct {
	SiteID   uint    `json:"site_id"`
	SiteName string  `json:"site_name"`
	SiteURL  string  `json:"site_url"`
	Revenue  float64 `json:"revenue"`
	Duration float64 `json:"duration_seconds"`
	Message  string  `json:"message"`
}

// TestSite 用已保存的凭据打一次该站点的日志统计，验证管理员权限可用。
func (s *Service) TestSite(ctx context.Context, id uint) (*TestResult, error) {
	site, err := s.sites.FindByID(id)
	if err != nil {
		return nil, notFoundOr(err, "读取站点")
	}
	stat, ch, err := s.statConnectorFor(site)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	end := time.Now()
	begin := end.Add(-24 * time.Hour)
	res, err := stat.GetLogStat(ctx, ch, s.sessionFor(site.UserID, site.AccessTokenCipher, site.CookieCipher), connector.LogStatQuery{
		Type:  connector.LogStatTypeConsume,
		Start: begin,
		End:   end,
	})
	if err != nil {
		return nil, fmt.Errorf("读取站点日志统计失败（需要管理员权限）: %w", err)
	}
	return &TestResult{
		SiteID:   site.ID,
		SiteName: siteDisplayName(site),
		SiteURL:  ch.SiteURL,
		Revenue:  res.Amount,
		Duration: time.Since(started).Seconds(),
		Message:  fmt.Sprintf("最近 24 小时全站消费 %0.4f，管理员权限可用", res.Amount),
	}, nil
}

// Summary 顶部卡片与弹窗读取的收益结果。
//
// Revenue / AdminCost 是全部启用站点的合计，Sites 是各站点自己的明细；
// NetProfit = Revenue - AdminCost - ChannelCost，渠道成本全局只减一次。
type Summary struct {
	Configured bool `json:"configured"`
	Enabled    bool `json:"enabled"`
	// SiteURL 第一个启用站点的地址，兼容只展示单站的旧视图。
	SiteURL       string  `json:"site_url"`
	SiteCount     int     `json:"site_count"`
	SiteFailed    int     `json:"site_failed"`
	Day           string  `json:"day"`
	HasSnapshot   bool    `json:"has_snapshot"`
	Revenue       float64 `json:"revenue"`
	AdminCost     float64 `json:"admin_cost"`
	ChannelCost   float64 `json:"channel_cost"`
	NetProfit     float64 `json:"net_profit"`
	RevenueOK     bool    `json:"revenue_ok"`
	AdminOK       bool    `json:"admin_ok"`
	ChannelOK     int     `json:"channel_ok"`
	ChannelFailed int     `json:"channel_failed"`
	// Sites 各站点自己的收益；快照里没有的站点按「未计算」展示（金额 0）。
	Sites          []storage.ProfitSiteProfit  `json:"sites"`
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
	sites, err := s.sites.ListEnabled()
	if err != nil {
		return nil, err
	}
	out := &Summary{
		Configured: anySiteConfigured(sites),
		Enabled:    set.Enabled,
		Day:        dayKey(time.Now()),
		Running:    s.isRunning(),
	}
	if len(sites) > 0 {
		out.SiteURL = sites[0].SiteURL
	}

	snap, err := s.snapshots.LatestForDay(out.Day)
	if err != nil {
		return nil, err
	}
	if snap == nil {
		out.Sites = mergeSiteProfits(sites, nil)
		out.SiteCount = len(out.Sites)
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
	out.Sites = mergeSiteProfits(sites, storage.DecodeProfitSites(snap.SiteProfitsJSON))
	out.SiteCount = len(out.Sites)
	for i := range out.Sites {
		if siteFailedResult(&out.Sites[i]) {
			out.SiteFailed++
		}
	}
	sampledAt := snap.SampledAt
	out.SampledAt = &sampledAt
	out.SnapshotFailed = snap.ChannelFailed
	return out, nil
}

// Refresh 跑一轮完整收益更新：逐站点总收益 / 管理员消费 → 逐供应商今日消费。
func (s *Service) Refresh(ctx context.Context) (*Summary, error) {
	return s.run(ctx, nil)
}

// Retry 只重试指定渠道，站点金额沿用上一轮快照，未重试的渠道沿用其上次成功金额。
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
	if !set.Enabled {
		return nil, ErrDisabled
	}
	allSites, err := s.sites.ListEnabled()
	if err != nil {
		return nil, err
	}
	sites := make([]storage.ProfitSite, 0, len(allSites))
	for i := range allSites {
		if siteConfigured(&allSites[i]) {
			sites = append(sites, allSites[i])
		}
	}
	if len(sites) == 0 {
		return nil, ErrNotConfigured
	}

	now := time.Now()
	day := dayKey(now)
	prev, err := s.snapshots.LatestForDay(day)
	if err != nil {
		return nil, err
	}
	// 重试模式下不碰任何站点：一次渠道重试不该把全部站点再打一遍（也可能把站点打挂）。
	reuseSites := retryIDs != nil && prev != nil

	prevSites := make(map[uint]storage.ProfitSiteProfit, len(sites))
	if prev != nil {
		for _, p := range storage.DecodeProfitSites(prev.SiteProfitsJSON) {
			prevSites[p.SiteID] = p
		}
	}

	base := progress.FromContext(ctx)
	siteProfits := make([]storage.ProfitSiteProfit, 0, len(sites))
	siteFailed := 0
	var revenueTotal, adminTotal float64
	revenueAllOK, adminAllOK := true, true

	for i := range sites {
		site := &sites[i]
		out := storage.ProfitSiteProfit{SiteID: site.ID, SiteName: siteDisplayName(site), SiteURL: site.SiteURL}
		previous, hasPrev := prevSites[site.ID]

		if reuseSites {
			out.Revenue, out.AdminCost = previous.Revenue, previous.AdminCost
			out.RevenueOK, out.AdminOK = previous.RevenueOK, previous.AdminOK
			out.Error = previous.Error
			if hasPrev {
				progress.OK(ctx, progress.StageRevenue, fmt.Sprintf("%s：沿用本轮总收益 %0.4f", out.SiteName, out.Revenue))
				progress.OK(ctx, progress.StageAdminCost, fmt.Sprintf("%s：沿用本轮管理员消费 %0.4f", out.SiteName, out.AdminCost))
			}
			out.SiteProfit = round4(out.Revenue - out.AdminCost)
			revenueTotal += out.Revenue
			adminTotal += out.AdminCost
			revenueAllOK = revenueAllOK && out.RevenueOK
			adminAllOK = adminAllOK && out.AdminOK
			if siteFailedResult(&out) {
				siteFailed++
			}
			siteProfits = append(siteProfits, out)
			continue
		}

		sctx := progress.WithObserver(ctx, siteScopedObserver{
			base:     base,
			siteID:   site.ID,
			siteName: out.SiteName,
			index:    i + 1,
			total:    len(sites),
		})

		stat, ch, cerr := s.statConnectorFor(site)
		if cerr != nil {
			out.Error = cerr.Error()
			out.Revenue, out.AdminCost = previous.Revenue, previous.AdminCost
			progress.Fail(sctx, progress.StageRevenue, fmt.Sprintf("%s：%v", out.SiteName, cerr))
			siteFailed++
		} else {
			session := s.sessionFor(site.UserID, site.AccessTokenCipher, site.CookieCipher)

			progress.Start(sctx, progress.StageRevenue, fmt.Sprintf("%s：拉取今日总收益…", out.SiteName))
			res, rerr := stat.GetLogStat(sctx, ch, session, todayQuery(now))
			if rerr != nil {
				out.Revenue = previous.Revenue
				progress.Fail(sctx, progress.StageRevenue, fmt.Sprintf("%s：总收益获取失败：%v", out.SiteName, rerr))
				if out.Error == "" {
					out.Error = fmt.Sprintf("总收益获取失败：%v", rerr)
				}
			} else {
				out.Revenue, out.RevenueOK = round4(res.Amount), true
				progress.OK(sctx, progress.StageRevenue, fmt.Sprintf("%s：今日总收益 %0.4f", out.SiteName, out.Revenue), map[string]any{"revenue": out.Revenue})
			}

			progress.Start(sctx, progress.StageAdminCost, fmt.Sprintf("%s：拉取管理员测试消费…", out.SiteName))
			self, serr := stat.GetSelfLogStat(sctx, ch, session, todayQuery(now))
			if serr != nil {
				out.AdminCost = previous.AdminCost
				progress.Fail(sctx, progress.StageAdminCost, fmt.Sprintf("%s：管理员消费获取失败：%v", out.SiteName, serr))
				if out.Error == "" {
					out.Error = fmt.Sprintf("管理员消费获取失败：%v", serr)
				}
			} else {
				out.AdminCost, out.AdminOK = round4(self.Amount), true
				progress.OK(sctx, progress.StageAdminCost, fmt.Sprintf("%s：管理员消费 %0.4f", out.SiteName, out.AdminCost), map[string]any{"admin_cost": out.AdminCost})
			}
		}

		out.SiteProfit = round4(out.Revenue - out.AdminCost)
		revenueTotal += out.Revenue
		adminTotal += out.AdminCost
		revenueAllOK = revenueAllOK && out.RevenueOK
		adminAllOK = adminAllOK && out.AdminOK
		if siteFailedResult(&out) {
			siteFailed++
		}
		siteProfits = append(siteProfits, out)
	}

	revenue := round4(revenueTotal)
	adminCost := round4(adminTotal)
	revenueOK, adminOK := revenueAllOK, adminAllOK

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
	net := round4(revenue - adminCost - channelCost)

	// 收口「成本」这一步：每个渠道的消费事件带 channel_id，这里用不带 channel_id
	// 的同名事件把汇总行标记为结束，避免前端进度列表在流程结束后仍留着转圈。
	progress.OK(ctx, progress.StageCost, fmt.Sprintf("成本合计 %0.4f", channelCost), map[string]any{"channel_cost": channelCost})

	snap := &storage.ProfitSnapshot{
		Day:             day,
		Revenue:         revenue,
		AdminCost:       adminCost,
		ChannelCost:     channelCost,
		NetProfit:       net,
		RevenueOK:       revenueOK,
		AdminOK:         adminOK,
		ChannelOK:       channelOK,
		ChannelFailed:   channelFailed,
		ChannelsJSON:    storage.EncodeProfitChannels(details),
		SiteProfitsJSON: storage.EncodeProfitSites(siteProfits),
		SampledAt:       time.Now(),
	}
	if err := s.snapshots.Create(snap); err != nil {
		return nil, fmt.Errorf("保存收益快照: %w", err)
	}

	summary := &Summary{
		Configured:     true,
		Enabled:        set.Enabled,
		SiteURL:        sites[0].SiteURL,
		SiteCount:      len(siteProfits),
		SiteFailed:     siteFailed,
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
		Sites:          siteProfits,
		Channels:       details,
		SampledAt:      &snap.SampledAt,
		SnapshotFailed: channelFailed,
	}

	msg := fmt.Sprintf("计算完成：总收益 %0.4f - 管理员消费 %0.4f - 成本 %0.4f = 净收益 %0.4f（%d 个站点）", revenue, adminCost, channelCost, net, len(siteProfits))
	payload := map[string]any{"summary": summary}
	switch {
	case siteFailed > 0 && channelFailed > 0:
		progress.FailData(ctx, progress.StageError, fmt.Sprintf("%s（%d 个站点、%d 个供应商采集失败，已按上次成功金额计入）", msg, siteFailed, channelFailed), payload)
	case siteFailed > 0:
		progress.FailData(ctx, progress.StageError, fmt.Sprintf("%s（%d 个站点采集失败，已按上次成功金额计入）", msg, siteFailed), payload)
	case channelFailed > 0:
		progress.FailData(ctx, progress.StageError, fmt.Sprintf("%s（%d 个供应商采集失败，已按上次成功金额计入）", msg, channelFailed), payload)
	case !revenueOK || !adminOK:
		progress.FailData(ctx, progress.StageError, fmt.Sprintf("%s（站点数据获取不完整）", msg), payload)
	default:
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

// siteFailedResult 该站点本轮是否有金额没取到（或站点整体不可用）。
func siteFailedResult(p *storage.ProfitSiteProfit) bool {
	return p.Error != "" || !p.RevenueOK || !p.AdminOK
}

// statConnectorFor 构造某个站点的连接器。收益统计只支持 NewAPI 系
// （/api/log/stat 是它的管理接口）。
func (s *Service) statConnectorFor(site *storage.ProfitSite) (connector.StatConnector, *connector.Channel, error) {
	if !siteConfigured(site) {
		return nil, nil, fmt.Errorf("%s：站点地址或凭据不完整", siteDisplayName(site))
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
	ch := &connector.Channel{
		Type:     connector.TypeNewAPI,
		SiteURL:  strings.TrimRight(site.SiteURL, "/"),
		Username: site.UserID,
	}
	return stat, ch, nil
}

// sessionFor 把加密保存的凭据还原成连接器会话。
func (s *Service) sessionFor(userID, tokenCipher, cookieCipher string) *connector.AuthSession {
	session := &connector.AuthSession{UserID: userID}
	if tokenCipher != "" {
		if token, err := s.cipher.Decrypt(tokenCipher); err == nil {
			session.AccessToken = token
		} else if s.log != nil {
			s.log.Warn("解密站点 access token 失败", "err", err)
		}
	}
	if cookieCipher != "" {
		if cookie, err := s.cipher.Decrypt(cookieCipher); err == nil {
			session.Cookie = cookie
		} else if s.log != nil {
			s.log.Warn("解密站点 cookie 失败", "err", err)
		}
	}
	return session
}

func configView(set *storage.ProfitSetting, sites []storage.ProfitSite) *ConfigView {
	out := &ConfigView{Enabled: set.Enabled, Sites: make([]SiteView, 0, len(sites))}
	for i := range sites {
		site := &sites[i]
		view := siteView(site)
		out.Sites = append(out.Sites, *view)
		if view.Enabled && view.Configured {
			out.Configured = true
		}
	}
	return out
}

func siteView(site *storage.ProfitSite) *SiteView {
	mode := string(site.CredentialMode)
	if mode == "" {
		mode = string(storage.ProfitCredentialToken)
	}
	return &SiteView{
		ID:             site.ID,
		Name:           siteDisplayName(site),
		SiteURL:        site.SiteURL,
		CredentialMode: mode,
		UserID:         site.UserID,
		Enabled:        site.Enabled,
		SortOrder:      site.SortOrder,
		HasToken:       site.AccessTokenCipher != "",
		HasCookie:      site.CookieCipher != "",
		Configured:     siteConfigured(site),
	}
}

// siteConfigured 站点地址与任一凭据齐备才算配置完成。
func siteConfigured(site *storage.ProfitSite) bool {
	if site == nil || strings.TrimSpace(site.SiteURL) == "" {
		return false
	}
	return site.AccessTokenCipher != "" || site.CookieCipher != ""
}

// anySiteConfigured 至少一个启用站点配置完成。
func anySiteConfigured(sites []storage.ProfitSite) bool {
	for i := range sites {
		if siteConfigured(&sites[i]) {
			return true
		}
	}
	return false
}

// mergeSiteProfits 把快照里的站点金额与当前启用站点对齐：
// 顺序按当前配置，站点名取最新，快照里没有的站点按「未计算」补零。
func mergeSiteProfits(current []storage.ProfitSite, stored []storage.ProfitSiteProfit) []storage.ProfitSiteProfit {
	byID := make(map[uint]storage.ProfitSiteProfit, len(stored))
	for _, p := range stored {
		byID[p.SiteID] = p
	}
	out := make([]storage.ProfitSiteProfit, 0, len(current))
	for i := range current {
		site := &current[i]
		p, ok := byID[site.ID]
		if !ok {
			p = storage.ProfitSiteProfit{SiteID: site.ID}
		}
		p.SiteName = siteDisplayName(site)
		p.SiteURL = site.SiteURL
		out = append(out, p)
	}
	return out
}

// siteDisplayName 站点展示名：用户填的优先，其次用域名，最后兜底 ID。
func siteDisplayName(site *storage.ProfitSite) string {
	if name := strings.TrimSpace(site.Name); name != "" {
		return name
	}
	if host := hostOf(site.SiteURL); host != "" {
		return host
	}
	return fmt.Sprintf("站点 %d", site.ID)
}

// fallbackSiteName 新建站点时没填名字就用域名。
func fallbackSiteName(siteURL string) string {
	if host := hostOf(siteURL); host != "" {
		return truncateRunes(host, 64)
	}
	return "自有站点"
}

func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}

func truncateRunes(v string, max int) string {
	runes := []rune(v)
	if len(runes) <= max {
		return v
	}
	return string(runes[:max])
}

func notFoundOr(err error, action string) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrSiteNotFound
	}
	return fmt.Errorf("%s: %w", action, err)
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

// siteScopedObserver 给站点级事件补上站点信息，弹窗据此按站点分行显示。
type siteScopedObserver struct {
	base     progress.Observer
	siteID   uint
	siteName string
	index    int
	total    int
}

func (o siteScopedObserver) Emit(ev progress.Event) {
	ev.SiteID = o.siteID
	ev.SiteName = o.siteName
	ev.Index = o.index
	ev.Total = o.total
	o.base.Emit(ev)
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
