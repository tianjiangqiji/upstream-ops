package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bejix/upstream-ops/backend/progress"
	"github.com/bejix/upstream-ops/backend/storage"
	"github.com/gin-gonic/gin"
)

// TestSyncChannelSkipsRatesWhenChannelOptsOut 覆盖渠道卡片的"同步"语义：
// 渠道开了「不采集分组倍率」后，同步只走余额 / 订阅用量，不再调用 RefreshRates。
func TestSyncChannelSkipsRatesWhenChannelOptsOut(t *testing.T) {
	cases := []struct {
		name    string
		skip    bool
		wantRun int
	}{
		{name: "开启开关", skip: true, wantRun: 0},
		{name: "关闭开关", skip: false, wantRun: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db := openTestDB(t)
			channels := storage.NewChannels(db)
			if err := channels.Create(&storage.Channel{
				Name:                   "skip-sync",
				Type:                   storage.ChannelTypeNewAPI,
				SiteURL:                "https://skip.example.com",
				Username:               "u1",
				PasswordCipher:         "x",
				MonitorEnabled:         true,
				SkipRatesOnAutoRefresh: tc.skip,
			}); err != nil {
				t.Fatalf("create channel: %v", err)
			}

			mon := &rateCountingMonitor{}
			r := gin.New()
			registerChannels(r.Group("/api"), &Deps{
				Channels: channels,
				Rates:    storage.NewRates(db),
				Notifies: storage.NewNotifications(db),
				Monitor:  mon,
			})

			req := httptest.NewRequest(http.MethodPost, "/api/channels/1/sync", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if mon.rateCalls != tc.wantRun {
				t.Fatalf("RefreshRates calls = %d, want %d", mon.rateCalls, tc.wantRun)
			}
			if mon.balanceCalls != 1 {
				t.Fatalf("RefreshBalance calls = %d, want 1", mon.balanceCalls)
			}
			if tc.skip && !strings.Contains(rec.Body.String(), "跳过分组倍率") {
				t.Fatalf("expected skip hint in sse body, got %s", rec.Body.String())
			}
		})
	}
}

type rateCountingMonitor struct {
	balanceCalls int
	rateCalls    int
}

func (m *rateCountingMonitor) RefreshBalance(ctx context.Context, c *storage.Channel) error {
	m.balanceCalls++
	progress.OK(ctx, progress.StageBalance, "ok")
	return nil
}

func (m *rateCountingMonitor) RefreshRates(ctx context.Context, c *storage.Channel) error {
	m.rateCalls++
	return nil
}

func (m *rateCountingMonitor) CheckSubscriptionUsageAlerts(ctx context.Context, c *storage.Channel) error {
	return nil
}

func (m *rateCountingMonitor) RefreshCosts(ctx context.Context, c *storage.Channel) error {
	progress.OK(ctx, progress.StageCost, "ok")
	return nil
}
