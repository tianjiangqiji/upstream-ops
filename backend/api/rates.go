package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/bejix/upstream-ops/backend/storage"
	"github.com/gin-gonic/gin"
)

func registerRates(g *gin.RouterGroup, d *Deps) {
	g.GET("/rate-changes", func(c *gin.Context) {
		var channelID uint
		if s := c.Query("channel_id"); s != "" {
			id, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				fail(c, http.StatusBadRequest, err)
				return
			}
			channelID = uint(id)
		}
		page, pageSize, err := parsePageQuery(c)
		if err != nil {
			fail(c, http.StatusBadRequest, err)
			return
		}
		list, total, err := d.Rates.ListChangesPage(channelID, page, pageSize)
		if err != nil {
			fail(c, http.StatusInternalServerError, err)
			return
		}
		pages := 1
		if total > 0 {
			pages = int((total + int64(pageSize) - 1) / int64(pageSize))
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"items":     list,
			"total":     total,
			"page":      page,
			"page_size": pageSize,
			"pages":     pages,
		}})
	})

	g.GET("/rates/batch", func(c *gin.Context) { batchChannelRates(c, d) })
}

// batchChannelRates 一次返回多个渠道的倍率快照。
//
// 渠道卡片网格原先给每张卡片各发一次 /channels/:id/rates：一屏 N 张卡片就是 N 个请求，
// 每 30s 刷新 tick 再全部重发一遍。改成批量接口后整个网格只发一个请求。
//
//	GET /api/rates/batch?ids=1,2,3&only_with_keys=2,3
//
// only_with_keys 是"仅显示已创建密钥的分组"开关打开的渠道 ID 列表，
// 语义与 /channels/:id/rates?only_with_keys=1 完全一致。
func batchChannelRates(c *gin.Context, d *Deps) {
	ids := parseIDList(c.Query("ids"))
	if len(ids) == 0 {
		c.JSON(http.StatusOK, gin.H{"data": map[uint][]storage.RateSnapshot{}})
		return
	}
	// 单次请求的渠道数上限，避免超长 URL 和超大响应把浏览器压死。
	const maxBatch = 200
	if len(ids) > maxBatch {
		ids = ids[:maxBatch]
	}

	byChannel, err := d.Rates.ListByChannels(ids)
	if err != nil {
		fail(c, http.StatusInternalServerError, err)
		return
	}

	onlyWithKeys := parseIDList(c.Query("only_with_keys"))
	if len(onlyWithKeys) > 0 {
		filtered := make(map[uint][]storage.RateSnapshot, len(ids))
		for _, id := range ids {
			if !containsID(onlyWithKeys, id) {
				filtered[id] = byChannel[id]
				continue
			}
			set, err := d.ChannelSvc.ListAPIKeyGroupSet(c.Request.Context(), id)
			if err != nil {
				fail(c, http.StatusInternalServerError, err)
				return
			}
			list := byChannel[id]
			if set.Empty() {
				filtered[id] = []storage.RateSnapshot{}
				continue
			}
			kept := make([]storage.RateSnapshot, 0, len(list))
			for _, snap := range list {
				if set.ContainsSnapshot(snap) {
					kept = append(kept, snap)
				}
			}
			filtered[id] = kept
		}
		byChannel = filtered
	}

	c.JSON(http.StatusOK, gin.H{"data": byChannel})
}

// parseIDList 解析 "1,2,3" 形式的 ID 列表，忽略空段与非法值，按出现顺序去重。
func parseIDList(raw string) []uint {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]uint, 0, len(parts))
	seen := make(map[uint]struct{}, len(parts))
	for _, part := range parts {
		n, err := strconv.ParseUint(strings.TrimSpace(part), 10, 64)
		if err != nil || n == 0 {
			continue
		}
		id := uint(n)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func containsID(ids []uint, target uint) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}
