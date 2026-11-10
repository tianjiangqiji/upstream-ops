package sub2api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bejix/upstream-ops/backend/connector"
)

func TestUsageStatsCostsDateRangeAndMultiplier(t *testing.T) {
	multiplier := 2.0
	for _, tc := range []struct {
		name       string
		now        time.Time
		day        string
		manualRate *float64
		mode       string
		wantToday  float64
		wantTotal  float64
	}{
		{"before midnight", time.Date(2026, 11, 6, 15, 59, 59, 0, time.UTC), "2026-11-06", nil, "divide", 0.3532, 3.5317},
		{"midnight with manual multiplier", time.Date(2026, 11, 6, 16, 0, 0, 0, time.UTC), "2026-11-07", &multiplier, "multiply", 1.4127, 14.1267},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var starts []string
			checkoutCalls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/prefix/api/v1/usage/stats":
					if r.Header.Get("Authorization") != "Bearer token" {
						t.Error("missing access token")
					}
					q := r.URL.Query()
					if q.Get("end_date") != tc.day || q.Get("timezone") != "Asia/Hong_Kong" || len(q) != 3 {
						t.Errorf("unexpected query: %v", q)
					}
					starts = append(starts, q.Get("start_date"))
					if q.Get("start_date") == tc.day {
						// Actual cost from the reported API example differs from list-price cost.
						_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"total_requests":57,"total_cost":5.0129296,"total_actual_cost":0.70633744}}`))
					} else {
						_, _ = w.Write([]byte(`{"code":0,"data":{"total_cost":50.129296,"total_actual_cost":7.0633744}}`))
					}
				case "/prefix/api/v1/payment/checkout-info":
					checkoutCalls++
					_, _ = w.Write([]byte(`{"code":0,"data":{"balance_recharge_multiplier":2}}`))
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			res, err := New().getUsageStatsCosts(context.Background(), &connector.Channel{
				SiteURL:                srv.URL + "/prefix/",
				RechargeMultiplier:     tc.manualRate,
				RechargeMultiplierMode: tc.mode,
			}, &connector.AuthSession{AccessToken: "token"}, tc.now)
			if err != nil {
				t.Fatalf("getUsageStatsCosts: %v", err)
			}
			if res.TodayCost != tc.wantToday || res.TotalCost != tc.wantTotal {
				t.Fatalf("costs = %#v, want today %v, total %v", res, tc.wantToday, tc.wantTotal)
			}
			if len(starts) != 2 || starts[0] != tc.day || starts[1] != "1970-01-01" {
				t.Fatalf("start dates = %v", starts)
			}
			if (tc.manualRate == nil && checkoutCalls != 1) || (tc.manualRate != nil && checkoutCalls != 0) {
				t.Fatalf("checkout calls = %d", checkoutCalls)
			}
		})
	}
}

func TestGetCostsUsageStatsHandlesZeroAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		totalOnly bool
		wantError string
	}{
		{"zero usage", 200, `{"code":0,"data":{"total_actual_cost":0}}`, false, ""},
		{"unauthorized", 401, `{"message":"expired"}`, false, "today"},
		{"stats unavailable", 404, `404 Not Found`, false, "today"},
		{"upstream error", 200, `{"code":1,"message":"stats disabled"}`, false, "stats disabled"},
		{"missing actual cost", 200, `{"code":0,"data":{"total_cost":5}}`, false, "missing total_actual_cost"},
		{"null actual cost", 200, `{"code":0,"data":{"total_actual_cost":null}}`, false, "missing total_actual_cost"},
		{"malformed data", 200, `{"code":0,"data":{"total_actual_cost":"bad"}}`, false, "decode"},
		{"cumulative stats failure", 500, `server error`, true, "total"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/usage/stats" {
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if tc.totalOnly && r.URL.Query().Get("start_date") != "1970-01-01" {
					_, _ = w.Write([]byte(`{"code":0,"data":{"total_actual_cost":1}}`))
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			multiplier := 1.0
			res, err := New().GetCosts(context.Background(), &connector.Channel{
				SiteURL:               srv.URL,
				UseUsageStatsForCosts: true,
				RechargeMultiplier:    &multiplier,
			}, &connector.AuthSession{AccessToken: "token"})
			if tc.wantError == "" {
				if err != nil || res == nil || res.TodayCost != 0 || res.TotalCost != 0 {
					t.Fatalf("zero usage: res=%#v, err=%v", res, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) || res != nil {
				t.Fatalf("res=%#v, err=%v, want error containing %q", res, err, tc.wantError)
			}
		})
	}
}
