package channel

import (
	"context"
	"testing"

	"github.com/bejix/upstream-ops/backend/storage"
)

func TestResolvePassesUsageStatsCostsSetting(t *testing.T) {
	svc, _ := testService(t)
	ch, err := svc.Create(CreateInput{
		Name:                  "usage-stats",
		Type:                  storage.ChannelTypeSub2API,
		SiteURL:               "https://example.com",
		Password:              "secret",
		UseUsageStatsForCosts: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := svc.Resolve(context.Background(), ch)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.UseUsageStatsForCosts {
		t.Fatal("resolved connector lost usage stats costs setting")
	}
}
