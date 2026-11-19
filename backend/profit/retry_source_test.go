package profit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bejix/upstream-ops/backend/storage"
)

// TestRetryKeepsSuccessfulChannelsOutOfFailureList 只重试失败渠道时，
// 上一轮已经成功的渠道不能因为「本轮没参与采集」就被标成 fallback/missing。
func TestRetryKeepsSuccessfulChannelsOutOfFailureList(t *testing.T) {
	site := newSiteStub(t)
	site.revenue = 10
	site.adminCost = 1
	fail := map[string]error{"broken": errors.New("登录失败")}
	svc, sites, _, channels := newTestService(t, &stubRefresher{fail: fail})
	addSite(t, svc, sites, "主站", site.server.URL)

	names := []string{"ok1", "ok2", "broken"}
	for _, name := range names {
		if err := channels.Create(&storage.Channel{Name: name, Type: storage.ChannelTypeNewAPI, SiteURL: "https://" + name + ".example.com", PasswordCipher: "x"}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	for i := range names {
		if err := channels.UpdateCosts(uint(i+1), float64(i+1), 0, time.Now()); err != nil {
			t.Fatalf("costs %s: %v", names[i], err)
		}
	}

	if _, err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// 修好 broken 后只重试它。
	delete(fail, "broken")
	retried, err := svc.Retry(context.Background(), []uint{3})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.ChannelFailed != 0 {
		t.Fatalf("channel failed = %d, want 0", retried.ChannelFailed)
	}
	for _, d := range retried.Channels {
		if d.ChannelName == "broken" {
			if d.Source != sourceFresh {
				t.Fatalf("broken source = %q, want %q", d.Source, sourceFresh)
			}
			continue
		}
		// ok1 / ok2 本轮没参与采集，应为 carried，而不是 fallback。
		if d.Source != sourceCarried {
			t.Fatalf("%s source = %q, want %q", d.ChannelName, d.Source, sourceCarried)
		}
	}
}

// TestRetryKeepsUnfixedFailure 上一轮就失败、本轮又没被重试的渠道，失败状态必须保留，
// 否则界面会把「还没修好」的供应商当成正常。
func TestRetryKeepsUnfixedFailure(t *testing.T) {
	site := newSiteStub(t)
	site.revenue = 10
	site.adminCost = 1
	fail := map[string]error{"broken": errors.New("登录失败")}
	svc, sites, _, channels := newTestService(t, &stubRefresher{fail: fail})
	addSite(t, svc, sites, "主站", site.server.URL)

	for _, name := range []string{"ok", "broken"} {
		if err := channels.Create(&storage.Channel{Name: name, Type: storage.ChannelTypeNewAPI, SiteURL: "https://" + name + ".example.com", PasswordCipher: "x"}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	if err := channels.UpdateCosts(1, 2.0, 0, time.Now()); err != nil {
		t.Fatalf("costs ok: %v", err)
	}
	if err := channels.UpdateCosts(2, 4.0, 0, time.Now()); err != nil {
		t.Fatalf("costs broken: %v", err)
	}

	first, err := svc.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if first.ChannelFailed != 1 {
		t.Fatalf("first channel failed = %d, want 1", first.ChannelFailed)
	}

	// 只重试 ok：broken 未参与，失败状态应保留。
	retried, err := svc.Retry(context.Background(), []uint{1})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.ChannelFailed != 1 {
		t.Fatalf("retried channel failed = %d, want 1", retried.ChannelFailed)
	}
	if retried.ChannelCost != 6.0 {
		t.Fatalf("channel cost = %v, want 6 (2 fresh + 4 沿用)", retried.ChannelCost)
	}
	for _, d := range retried.Channels {
		switch d.ChannelName {
		case "ok":
			if d.Source != sourceFresh {
				t.Fatalf("ok source = %q, want %q", d.Source, sourceFresh)
			}
		case "broken":
			if d.Source != sourceFallback {
				t.Fatalf("broken source = %q, want %q", d.Source, sourceFallback)
			}
			if d.Cost != 4.0 {
				t.Fatalf("broken cost = %v, want 4", d.Cost)
			}
		}
	}

	// 再重试一次 ok（已经成功）：失败项数量不能变多。
	again, err := svc.Retry(context.Background(), []uint{1})
	if err != nil {
		t.Fatalf("retry again: %v", err)
	}
	if again.ChannelFailed != 1 {
		t.Fatalf("channel failed = %d, want 1", again.ChannelFailed)
	}
	if len(again.Channels) != 2 {
		t.Fatalf("channels = %d, want 2", len(again.Channels))
	}
}
