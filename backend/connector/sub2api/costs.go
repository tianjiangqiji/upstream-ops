package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bejix/upstream-ops/backend/connector"
)

const usageStatsTimezone = "Asia/Hong_Kong"

// getUsageStatsCosts supports sites that only expose the usage-record statistics API.
func (c *Client) getUsageStatsCosts(ctx context.Context, ch *connector.Channel, session *connector.AuthSession, now time.Time) (*connector.CostResult, error) {
	// Hong Kong and the panel's daily cost statistics both use UTC+8.
	today := now.In(time.FixedZone(usageStatsTimezone, 8*60*60)).Format("2006-01-02")
	endpoint := strings.TrimRight(ch.SiteURL, "/") + "/api/v1/usage/stats"
	query := url.Values{
		"start_date": {today},
		"end_date":   {today},
		"timezone":   {usageStatsTimezone},
	}
	todayCost, err := c.getUsageStatsActualCost(ctx, endpoint+"?"+query.Encode(), session)
	if err != nil {
		return nil, fmt.Errorf("sub2api usage stats (today): %w", err)
	}
	// An omitted range can default to the last month. Specify the full available
	// history instead so the daily result is never mistaken for cumulative cost.
	query.Set("start_date", "1970-01-01")
	totalCost, err := c.getUsageStatsActualCost(ctx, endpoint+"?"+query.Encode(), session)
	if err != nil {
		return nil, fmt.Errorf("sub2api usage stats (total): %w", err)
	}
	multiplier := c.rechargeMultiplier(ctx, ch, session)
	return &connector.CostResult{
		TodayCost: connector.ApplyRechargeMultiplier(todayCost, multiplier, ch.RechargeMultiplierMode),
		TotalCost: connector.ApplyRechargeMultiplier(totalCost, multiplier, ch.RechargeMultiplierMode),
	}, nil
}

func (c *Client) getUsageStatsActualCost(ctx context.Context, endpoint string, session *connector.AuthSession) (float64, error) {
	body, err := c.getJSON(ctx, endpoint, session)
	if err != nil {
		return 0, err
	}
	var stats struct {
		TotalActualCost *float64 `json:"total_actual_cost"`
	}
	if err := json.Unmarshal(body, &stats); err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}
	if stats.TotalActualCost == nil {
		return 0, errors.New("missing total_actual_cost")
	}
	return *stats.TotalActualCost, nil
}
