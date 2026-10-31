package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bejix/upstream-ops/backend/storage"
)

// TestChannelSkipRatesOnAutoRefresh 覆盖「定时刷新只取额度、不取分组倍率」开关在 API 层的语义：
// 创建时可指定、读取时回显、局部更新不提供则保持不变、显式 false 能关掉。
func TestChannelSkipRatesOnAutoRefresh(t *testing.T) {
	r, channels := newChannelMetaRouter(t)

	body := baseChannelCreateBody("skip-demo")
	body["skip_rates_on_auto_refresh"] = true
	created := decodeSkipRates(t, doChannelJSON(t, r, http.MethodPost, "/api/channels", body))
	if !created.Skip {
		t.Fatal("create: skip_rates_on_auto_refresh = false, want true")
	}
	assertStoredSkipRates(t, channels, created.ID, true)

	path := fmt.Sprintf("/api/channels/%d", created.ID)

	// 局部更新（不带该字段）不应把它关掉
	updated := decodeSkipRates(t, doChannelJSON(t, r, http.MethodPut, path, `{"name":"skip-renamed"}`))
	if !updated.Skip {
		t.Fatal("partial update: skip_rates_on_auto_refresh = false, want true")
	}
	assertStoredSkipRates(t, channels, created.ID, true)

	// 显式 false 才关掉
	updated = decodeSkipRates(t, doChannelJSON(t, r, http.MethodPut, path, `{"skip_rates_on_auto_refresh":false}`))
	if updated.Skip {
		t.Fatal("explicit false: skip_rates_on_auto_refresh = true, want false")
	}
	assertStoredSkipRates(t, channels, created.ID, false)

	// 不提供该字段新建的渠道默认 false
	plain := decodeSkipRates(t, doChannelJSON(t, r, http.MethodPost, "/api/channels", baseChannelCreateBody("skip-plain")))
	if plain.Skip {
		t.Fatal("create plain: skip_rates_on_auto_refresh = true, want false")
	}
}

type channelSkipRatesOut struct {
	ID   uint `json:"id"`
	Skip bool `json:"skip_rates_on_auto_refresh"`
}

func decodeSkipRates(t *testing.T, rec *httptest.ResponseRecorder) channelSkipRatesOut {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data channelSkipRatesOut `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	return resp.Data
}

func assertStoredSkipRates(t *testing.T, channels *storage.Channels, id uint, want bool) {
	t.Helper()
	ch, err := channels.FindByID(id)
	if err != nil {
		t.Fatalf("find channel: %v", err)
	}
	if ch.SkipRatesOnAutoRefresh != want {
		t.Fatalf("stored skip_rates_on_auto_refresh = %v, want %v", ch.SkipRatesOnAutoRefresh, want)
	}
}
