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

	"github.com/bejix/upstream-ops/backend/crypto"
	_ "github.com/bejix/upstream-ops/backend/connector/newapi"
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

// siteStub 模拟 NewAPI 主站的 /api/status 与两个日志统计接口。
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

func newTestService(t *testing.T, siteURL string, refresher CostRefresher) (*Service, *storage.ProfitSettings, *storage.ProfitSnapshots, *storage.Channels) {
	t.Helper()
	db := openTestDB(t)
	settings := storage.NewProfitSettings(db)
	snapshots := storage.NewProfitSnapshots(db)
	channels := storage.NewChannels(db)
	cipher, err := crypto.NewCipher("profit-test-secret")
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	svc := NewService(settings, snapshots, channels, refresher, stubConfigurer{}, cipher, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if siteURL != "" {
		enc, err := cipher.Encrypt("admin-token")
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		if err := settings.Save(&storage.ProfitSetting{
			SiteURL:           siteURL,
			CredentialMode:    storage.ProfitCredentialToken,
			AccessTokenCipher: enc,
			UserID:            "1",
			Enabled:           true,
		}); err != nil {
			t.Fatalf("save setting: %v", err)
		}
	}
	return svc, settings, snapshots, channels
}

// TestRefreshComputesNetProfit 覆盖收益口径：总收益 - 管理员消费 - 各渠道今日消费。
func TestRefreshComputesNetProfit(t *testing.T) {
	site := newSiteStub(t)
	site.revenue = 12.5
	site.adminCost = 0.5
	svc, _, _, channels := newTestService(t, site.server.URL, &stubRefresher{})

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

// TestRefreshFallsBackToLastSuccess 失败渠道沿用上次成功金额，并在明细里标 fallback。
func TestRefreshFallsBackToLastSuccess(t *testing.T) {
	site := newSiteStub(t)
	site.revenue = 10
	site.adminCost = 1
	svc, _, _, channels := newTestService(t, site.server.URL, &stubRefresher{fail: map[string]error{"broken": errors.New("登录失败：密码错误")}})

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

// TestRefreshRequiresConfig 未配置主站时直接报 ErrNotConfigured，不落快照。
func TestRefreshRejectsDisabled(t *testing.T) {
	svc, _, _, _ := newTestService(t, "http://example.invalid", &stubRefresher{})
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

func TestRefreshRequiresConfig(t *testing.T) {
	svc, _, snapshots, _ := newTestService(t, "", &stubRefresher{})
	if _, err := svc.Refresh(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if snap, _ := snapshots.LatestForDay(dayKey(time.Now())); snap != nil {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
}

// TestRefreshRejectsConcurrent 同一时刻只允许一轮更新。
func TestRefreshRejectsConcurrent(t *testing.T) {
	svc, _, _, _ := newTestService(t, "", &stubRefresher{})
	svc.begin()
	if _, err := svc.Refresh(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v, want ErrAlreadyRunning", err)
	}
	svc.end()
}

// TestRetryKeepsSiteAmounts 重试只跑指定渠道，主站两个金额沿用上一轮。
func TestRetryKeepsSiteAmounts(t *testing.T) {
	site := newSiteStub(t)
	site.revenue = 8
	site.adminCost = 1
	svc, _, _, channels := newTestService(t, site.server.URL, &stubRefresher{})

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
	if _, err := svc.Retry(context.Background(), []uint{1}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(site.logPaths) != before {
		t.Fatalf("retry hit site %d times, want 0", len(site.logPaths)-before)
	}
}

// TestSummaryWithoutSnapshot 没算过时只返回配置状态，金额为 0。
func TestSummaryWithoutSnapshot(t *testing.T) {
	svc, _, _, _ := newTestService(t, "https://site.example.com", &stubRefresher{})
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
}
