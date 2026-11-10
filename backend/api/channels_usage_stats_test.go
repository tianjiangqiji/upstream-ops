package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/bejix/upstream-ops/backend/storage"
)

func TestChannelUsageStatsCostsSetting(t *testing.T) {
	r, channels := newChannelMetaRouter(t)
	request := func(method, path string, body any, want bool) uint {
		t.Helper()
		rec := doChannelJSON(t, r, method, path, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d, body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Data struct {
				ID      uint  `json:"id"`
				Enabled *bool `json:"use_usage_stats_for_costs"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Data.Enabled == nil || *resp.Data.Enabled != want {
			t.Fatalf("expected use_usage_stats_for_costs=%v, body=%s", want, rec.Body.String())
		}
		stored, err := channels.FindByID(resp.Data.ID)
		if err != nil || stored.UseUsageStatsForCosts != want {
			t.Fatalf("stored=%#v, err=%v", stored, err)
		}
		return resp.Data.ID
	}

	// Existing clients omit the setting and retain the dashboard endpoint.
	id := request(http.MethodPost, "/api/channels", baseChannelCreateBody("usage-default"), false)
	path := fmt.Sprintf("/api/channels/%d", id)
	request(http.MethodPut, path, map[string]any{"use_usage_stats_for_costs": true}, true)
	request(http.MethodGet, path, nil, true)
	request(http.MethodPut, path, map[string]any{"name": "usage-renamed"}, true)
	request(http.MethodPut, path, map[string]any{"use_usage_stats_for_costs": false}, false)

	body := baseChannelCreateBody("usage-enabled")
	body["use_usage_stats_for_costs"] = true
	request(http.MethodPost, "/api/channels", body, true)

	// Plugins that do not implement this mode cannot enable it.
	body = baseChannelCreateBody("usage-newapi")
	body["type"] = string(storage.ChannelTypeNewAPI)
	body["use_usage_stats_for_costs"] = true
	id = request(http.MethodPost, "/api/channels", body, false)
	request(http.MethodPut, fmt.Sprintf("/api/channels/%d", id), map[string]any{"use_usage_stats_for_costs": true}, false)
}
