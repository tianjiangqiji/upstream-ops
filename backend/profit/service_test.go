package profit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "github.com/bejix/upstream-ops/backend/connector/newapi"
	"github.com/bejix/upstream-ops/backend/crypto"
	"github.com/bejix/upstream-ops/backend/progress"
	"github.com/bejix/upstream-ops/backend/storage"
	"gorm.io/gorm"
)

// stubRefresher 按渠道名决定本轮消费采集是否成功。
//
// 真实实现（monitor.Service.RefreshCosts）成功时会把今日消费写回渠道，
// 这里由用例先用 channels.UpdateCosts 预置，再走收益计算读取同一个字段。
type stubRefresher struct {
	fail map[string]error
}

func (s *stubRefresher) RefreshCosts(ctx context.Context, c *storage.Channel) error {
	if err, ok := s.fail[c.Name]; ok && err != nil {
		progress.Fail(ctx, progress.StageCost, err.Error())
		return err
	}
	progress.OK(ctx, progress.StageCost, "ok")
	return nil
}

// stubConfigurer 模拟 channel.Service 的全局上游配置注入。
type stubConfigurer struct{}

func (stubConfigurer) ApplyHTTPConfig(any) {}

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := storage.Open(storage.DBConfig{Driver: storage.DBDriverSQLite, Path: filepath.Join(t.TempDir(), "profit-test.db")})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := storage.AutoMigrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// siteStub 模拟一个 NewAPI 自有站点的 /api/status 与两个日志统计接口。
type siteStub struct {
	server    *httptest.Server
	logTypes  []string
	logPaths  []string
	revenue   float64
	adminCost float64
	statusErr error
	logErr    error
}

func newSiteStub(t *testing.T) *siteStub {
	t.Helper()
	s := &siteStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if s.statusErr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"success":false,"message":"status boom"}`)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
	})
	handleStat := func(path string, amount func() float64) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.logPaths = append(s.logPaths, path)
			s.logTypes = append(s.logTypes, r.URL.Query().Get("type"))
			if s.logErr != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"success":false,"message":"stat boom"}`)
				return
			}
			quota := int(amount() * 500000)
			_, _ = fmt.Fprintf(w, `{"success":true,"data":{"quota":%d,"rpm":0,"tpm":0}}`, quota)
		}
	}
	mux.HandleFunc("/api/log/stat", handleStat("/api/log/stat", func() float64 { return s.revenue }))
	mux.HandleFunc("/api/log/self/stat", handleStat("/api/log/self/stat", func() float64 { return s.adminCost }))
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

// newTestService 建一个空的收益服务；站点由用例自己通过 sites.Create 添加。
func newTestService(t *testing.T, refresher CostRefresher) (*Service, *storage.ProfitSites, *storage.ProfitSnapshots, *storage.Channels) {
	t.Helper()
	db := openTestDB(t)
	settings := storage.NewProfitSettings(db)
	sites := storage.NewProfitSites(db)
	snapshots := storage.NewProfitSnapshots(db)
	channels := storage.NewChannels(db)
	cipher, err := crypto.NewCipher("profit-test-secret")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	svc := NewService(settings, sites, snapshots, channels, refresher, stubConfigurer{}, cipher, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// 总开关默认关闭（与全新安装一致），用例里显式打开。
	if err := settings.Save(&storage.ProfitSetting{Enabled: true}); err != nil {
		t.Fatalf("save setting: %v", err)
	}
	return svc, sites, snapshots, channels
}

// addSite 保存一个启用的自有站点（凭据用默认 token）。
func addSite(t *testing.T, svc *Service, sites *storage.ProfitSites, name, siteURL string) *storage.ProfitSite {
	t.Helper()
	enc, err := svc.cipher.Encrypt("admin-token")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	site := &storage.ProfitSite{
		Name:              name,
		SiteURL:           siteURL,
		CredentialMode:    storage.ProfitCredentialToken,
		AccessTokenCipher: enc,
		UserID:            "1",
		Enabled:           true,
	}
	if err := sites.Create(site); err != nil {
		t.Fatalf("create site %s: %v", name, err)
	}
	return site
}

// TestRefreshComputesNetProfit 覆盖收益口径：总收益 - 管理员消费 - 各渠道今日消费。
func TestRefreshComputesNetProfit(t *testing.T) {
	site := newSiteStub(t)
	site.revenue = 12.5
	site.adminCost = 0.5
	svc, sites, _, channels := newTestService(t, &stubRefresher{})
	addSite(t, svc, sites, "主站", site.server.URL)

	costA, costB := 3.0, 2.0
	if err := channels.Create(&storage.Channel{Name: "a", Type: storage.ChannelTypeNewAPI, SiteURL: "https://a.example.com", PasswordCipher: "x"}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := channels.Create(&storage.Channel{Name: "b", Type: storage.ChannelTypeNewAPI, SiteURL: "https://b.example.com", PasswordCipher: "x"}); err != nil {
		t.Fatalf("create b: %v", err)
	}
	// 渠道采集成功时，monitor 会把今日消费写回渠道；这里直接模拟写回结果。
	if err := channels.UpdateCosts(1, costA, 0, time.Now()); err != nil {
		t.Fatalf("update costs a: %v", err)
	}
	if err := channels.UpdateCosts(2, costB, 0, time.Now()); err != nil {
		t.Fatalf("update costs b: %v", err)
	}

	summary, err := svc.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if summary.Revenue != 12.5 {
		t.Fatalf("revenue = %v, want 12.5", summary.Revenue)
	}
	if summary.AdminCost != 0.5 {
		t.Fatalf("admin cost = %v, want 0.5", summary.AdminCost)
	}
	if summary.ChannelCost != 5.0 {
		t.Fatalf("channel cost = %v, want 5", summary.ChannelCost)
	}
	if summary.NetProfit != 7.0 {
		t.Fatalf("net profit = %v, want 7", summary.NetProfit)
	}
	if summary.ChannelOK != 2 || summary.ChannelFailed != 0 {
		t.Fatalf("channel ok/failed = %d/%d, want 2/0", summary.ChannelOK, summary.ChannelFailed)
	}
	// 消费统计必须显式传 type=2，否则充值 / 退款会被算进收益。
	for i, typ := range site.logTypes {
		if typ != strconv.Itoa(2) {
			t.Fatalf("log stat #%d type = %q, want 2", i, typ)
		}
	}
}

// TestRefreshAggregatesMultipleSites 多个自有站点各自计算收益，顶层金额是合计。
func TestRefreshAggregatesMultipleSites(t *testing.T) {
	first := newSiteStub(t)
	first.revenue = 10
	first.adminCost = 1
	second := newSiteStub(t)
	second.revenue = 4.5
	second.adminCost = 0.5

	svc, sites, _, channels := newTestService(t, &stubRefresher{})
	addSite(t, svc, sites, "一号站", first.server.URL)
	addSite(t, svc, sites, "二号站", second.server.URL)
	if err := channels.Create(&storage.Channel{Name: "a", Type: storage.ChannelTypeNewAPI, SiteURL: "https://a.example.com", PasswordCipher: "x"}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := channels.UpdateCosts(1, 2.0, 0, time.Now()); err != nil {
		t.Fatalf("costs a: %v", err)
	}

	summary, err := svc.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if summary.SiteCount != 2 {
		t.Fatalf("site count = %d, want 2", summary.SiteCount)
	}
	if summary.Revenue != 14.5 {
		t.Fatalf("revenue = %v, want 14.5", summary.Revenue)
	}
	if summary.AdminCost != 1.5 {
		t.Fatalf("admin cost = %v, want 1.5", summary.AdminCost)
	}
	// 渠道成本全局只减一次：14.5 - 1.5 - 2 = 11。
	if summary.NetProfit != 11 {
		t.Fatalf("net profit = %v, want 11", summary.NetProfit)
	}
	byName := map[string]storage.ProfitSiteProfit{}
	for _, p := range summary.Sites {
		byName[p.SiteName] = p
	}
	if got := byName["一号站"].SiteProfit; got != 9 {
		t.Fatalf("一号站 profit = %v, want 9", got)
	}
	if got := byName["二号站"].SiteProfit; got != 4 {
		t.Fatalf("二号站 profit = %v, want 4", got)
	}

	// 快照落库后 Summary 仍然给出两站的明细（顺序按站点配置）。
	got, err := svc.Summary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(got.Sites) != 2 || got.Sites[0].SiteProfit != 9 || got.Sites[1].SiteProfit != 4 {
		t.Fatalf("summary sites = %+v", got.Sites)
	}
	if got.SiteFailed != 0 {
		t.Fatalf("site failed = %d, want 0", got.SiteFailed)
	}
}

// TestRefreshKeepsOtherSitesWhenOneFails 单个站点失败不影响其它站点，也不阻断渠道计算。
func TestRefreshKeepsOtherSitesWhenOneFails(t *testing.T) {
	ok := newSiteStub(t)
	ok.revenue = 6
	ok.adminCost = 1
	broken := newSiteStub(t)
	broken.logErr = errors.New("stat boom")

	svc, sites, _, channels := newTestService(t, &stubRefresher{})
	addSite(t, svc, sites, "正常站", ok.server.URL)
	addSite(t, svc, sites, "故障站", broken.server.URL)
	if err := channels.Create(&storage.Channel{Name: "a", Type: storage.ChannelTypeNewAPI, SiteURL: "https://a.example.com", PasswordCipher: "x"}); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := channels.UpdateCosts(1, 1.0, 0, time.Now()); err != nil {
		t.Fatalf("costs a: %v", err)
	}

	summary, err := svc.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if summary.SiteFailed != 1 {
		t.Fatalf("site failed = %d, want 1", summary.SiteFailed)
	}
	if summary.Revenue != 6 {
		t.Fatalf("revenue = %v, want 6（故障站按 0 计入）", summary.Revenue)
	}
	if summary.NetProfit != 4 {
		t.Fatalf("net profit = %v, want 4", summary.NetProfit)
	}
	for _, p := range summary.Sites {
		if p.SiteName == "故障站" && p.Error == "" {
			t.Fatalf("故障站缺少错误说明: %+v", p)
		}
	}
}

// TestRefreshFallsBackToLastSuccess 失败渠道沿用上次成功金额，并在明细里标 fallback。
func TestRefreshFallsBackToLastSuccess(t *testing.T) {
	site := newSiteStub(t)
	site.revenue = 10
	site.adminCost = 1
	svc, sites, _, channels := newTestService(t, &stubRefresher{fail: map[string]error{"broken": errors.New("登录失败：密码错误")}})
	addSite(t, svc, sites, "主站", site.server.URL)

	if err := channels.Create(&storage.Channel{Name: "ok", Type: storage.ChannelTypeNewAPI, SiteURL: "https://ok.example.com", PasswordCipher: "x"}); err != nil {
		t.Fatalf("create ok: %v", err)
	}
	if err := channels.Create(&storage.Channel{Name: "broken", Type: storage.ChannelTypeNewAPI, SiteURL: "https://broken.example.com", PasswordCipher: "x"}); err != nil {
		t.Fatalf("create broken: %v", err)
	}
	if err := channels.UpdateCosts(1, 2.0, 0, time.Now()); err != nil {
		t.Fatalf("update ok costs: %v", err)
	}
	// 失败渠道上一次成功采集的金额留在库里，本轮应被沿用。
	if err := channels.UpdateCosts(2, 4.0, 0, time.Now()); err != nil {
		t.Fatalf("update broken costs: %v", err)
	}

	summary, err := svc.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if summary.ChannelCost != 6.0 {
		t.Fatalf("channel cost = %v, want 6 (2 + 4 fallback)", summary.ChannelCost)
	}
	if summary.ChannelFailed != 1 {
		t.Fatalf("channel failed = %d, want 1", summary.ChannelFailed)
	}
	if summary.NetProfit != 3.0 {
		t.Fatalf("net profit = %v, want 3", summary.NetProfit)
	}
	var broken *storage.ProfitChannelCost
	for i := range summary.Channels {
		if summary.Channels[i].ChannelName == "broken" {
			broken = &summary.Channels[i]
		}
	}
	if broken == nil {
		t.Fatalf("missing broken channel detail: %+v", summary.Channels)
	}
	if broken.Source != sourceFallback {
		t.Fatalf("broken source = %q, want %q", broken.Source, sourceFallback)
	}
}

// TestRefreshRejectsDisabled 总开关关闭时直接报 ErrDisabled。
func TestRefreshRejectsDisabled(t *testing.T) {
	site := newSiteStub(t)
	svc, sites, _, _ := newTestService(t, &stubRefresher{})
	addSite(t, svc, sites, "主站", site.server.URL)
	set, err := svc.settings.Get()
	if err != nil {
		t.Fatalf("get setting: %v", err)
	}
	set.Enabled = false
	if err := svc.settings.Save(set); err != nil {
		t.Fatalf("save setting: %v", err)
	}
	if _, err := svc.Refresh(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

// TestRefreshRequiresConfig 没有任何可用站点时直接报 ErrNotConfigured，不落快照。
func TestRefreshRequiresConfig(t *testing.T) {
	svc, sites, snapshots, _ := newTestService(t, &stubRefresher{})
	// 只有地址没有凭据的站点不算可用站点。
	if err := sites.Create(&storage.ProfitSite{Name: "空站", SiteURL: "https://empty.example.com", Enabled: true}); err != nil {
		t.Fatalf("create site: %v", err)
	}
	if _, err := svc.Refresh(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if snap, _ := snapshots.LatestForDay(dayKey(time.Now())); snap != nil {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

// TestRefreshSkipsDisabledSites 停用的站点不参与计算。
func TestRefreshSkipsDisabledSites(t *testing.T) {
	active := newSiteStub(t)
	active.revenue = 3
	active.adminCost = 0
	paused := newSiteStub(t)
	paused.revenue = 100
	paused.adminCost = 0

	svc, sites, _, _ := newTestService(t, &stubRefresher{})
	addSite(t, svc, sites, "启用站", active.server.URL)
	pausedSite := addSite(t, svc, sites, "停用站", paused.server.URL)
	pausedSite.Enabled = false
	if err := sites.Update(pausedSite); err != nil {
		t.Fatalf("update site: %v", err)
	}

	summary, err := svc.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if summary.SiteCount != 1 {
		t.Fatalf("site count = %d, want 1", summary.SiteCount)
	}
	if summary.Revenue != 3 {
		t.Fatalf("revenue = %v, want 3", summary.Revenue)
	}
	if len(paused.logPaths) != 0 {
		t.Fatalf("disabled site was called %d times", len(paused.logPaths))
	}
}

// TestRefreshRejectsConcurrent 同一时刻只允许一轮更新。
func TestRefreshRejectsConcurrent(t *testing.T) {
	svc, _, _, _ := newTestService(t, &stubRefresher{})
	svc.begin()
	if _, err := svc.Refresh(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
	svc.end()
}

// TestRetryKeepsSiteAmounts 重试只跑指定渠道，站点金额沿用上一轮。
func TestRetryKeepsSiteAmounts(t *testing.T) {
	site := newSiteStub(t)
	site.revenue = 8
	site.adminCost = 1
	svc, sites, _, channels := newTestService(t, &stubRefresher{})
	addSite(t, svc, sites, "主站", site.server.URL)

	for _, name := range []string{"a", "b"} {
		if err := channels.Create(&storage.Channel{Name: name, Type: storage.ChannelTypeNewAPI, SiteURL: "https://" + name + ".example.com", PasswordCipher: "x"}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if err := channels.UpdateCosts(1, 1.0, 0, time.Now()); err != nil {
		t.Fatalf("costs a: %v", err)
	}
	if err := channels.UpdateCosts(2, 2.0, 0, time.Now()); err != nil {
		t.Fatalf("costs b: %v", err)
	}
	if _, err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	before := len(site.logPaths)
	retried, err := svc.Retry(context.Background(), []uint{1})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(site.logPaths) != before {
		t.Fatalf("retry hit site %d times, want 0", len(site.logPaths)-before)
	}
	if retried.Revenue != 8 || retried.AdminCost != 1 {
		t.Fatalf("retried site amounts = %v/%v, want 8/1", retried.Revenue, retried.AdminCost)
	}
	if len(retried.Sites) != 1 || retried.Sites[0].SiteProfit != 7 {
		t.Fatalf("retried sites = %+v", retried.Sites)
	}
}

// TestSummaryWithoutSnapshot 没算过时只返回配置状态，金额为 0，站点仍列出。
func TestSummaryWithoutSnapshot(t *testing.T) {
	site := newSiteStub(t)
	svc, sites, _, _ := newTestService(t, &stubRefresher{})
	addSite(t, svc, sites, "主站", site.server.URL)

	summary, err := svc.Summary()
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.HasSnapshot {
		t.Fatalf("unexpected snapshot: %+v", summary)
	}
	if !summary.Configured {
		t.Fatalf("configured = false, want true")
	}
	if len(summary.Sites) != 1 || summary.Sites[0].SiteProfit != 0 {
		t.Fatalf("sites = %+v, want 一条零值站点", summary.Sites)
	}
}

// TestSiteCRUD 站点增删改与视图字段。
func TestSiteCRUD(t *testing.T) {
	svc, _, _, _ := newTestService(t, &stubRefresher{})

	created, err := svc.CreateSite(SiteInput{SiteURL: "https://a.example.com/", AccessToken: "tok", UserID: "1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Name != "a.example.com" {
		t.Fatalf("name = %q, want 域名兜底", created.Name)
	}
	if created.SiteURL != "https://a.example.com" {
		t.Fatalf("site url = %q, want 去掉尾部斜杠", created.SiteURL)
	}
	if !created.Enabled || !created.Configured || !created.HasToken || created.HasCookie {
		t.Fatalf("view = %+v", created)
	}

	// 只改名字：凭据必须保留。
	enabled := false
	updated, err := svc.UpdateSite(created.ID, SiteInput{SiteURL: created.SiteURL, Name: "备用站", Enabled: &enabled})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "备用站" || !updated.HasToken || updated.Enabled {
		t.Fatalf("updated = %+v", updated)
	}

	cfg, err := svc.Config()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if len(cfg.Sites) != 1 || cfg.Configured {
		t.Fatalf("config = %+v（唯一站点已停用，不应算已配置）", cfg)
	}

	if err := svc.DeleteSite(created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.DeleteSite(created.ID); !errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("delete again err = %v, want ErrSiteNotFound", err)
	}
}

// TestCreateSiteValidation 站点地址与凭据模式校验。
func TestCreateSiteValidation(t *testing.T) {
	svc, _, _, _ := newTestService(t, &stubRefresher{})
	if _, err := svc.CreateSite(SiteInput{}); err == nil {
		t.Fatalf("want error for empty site url")
	}
	if _, err := svc.CreateSite(SiteInput{SiteURL: "ftp://a.example.com"}); err == nil {
		t.Fatalf("want error for bad scheme")
	}
	if _, err := svc.CreateSite(SiteInput{SiteURL: "https://a.example.com", CredentialMode: "oauth"}); err == nil {
		t.Fatalf("want error for unknown credential mode")
	}
}

// TestTestSite 测试连接只打指定站点，返回该站最近 24 小时消费。
func TestTestSite(t *testing.T) {
	first := newSiteStub(t)
	first.revenue = 2.25
	second := newSiteStub(t)
	second.revenue = 99
	svc, sites, _, _ := newTestService(t, &stubRefresher{})
	addSite(t, svc, sites, "一号站", first.server.URL)
	secondSite := addSite(t, svc, sites, "二号站", second.server.URL)

	res, err := svc.TestSite(context.Background(), secondSite.ID)
	if err != nil {
		t.Fatalf("test site: %v", err)
	}
	if res.SiteName != "二号站" || res.Revenue != 99 {
		t.Fatalf("result = %+v", res)
	}
	if len(first.logPaths) != 0 {
		t.Fatalf("other site was called %d times", len(first.logPaths))
	}
	if _, err := svc.TestSite(context.Background(), 9999); !errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("err = %v, want ErrSiteNotFound", err)
	}
}
