package storage

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// ProfitSettings 主站（管理员自有站点）配置仓库，表里只有一行（ID=1）。
type ProfitSettings struct{ db *gorm.DB }

func NewProfitSettings(db *gorm.DB) *ProfitSettings { return &ProfitSettings{db: db} }

// profitSettingID 单行配置的固定主键。
const profitSettingID uint = 1

// Get 读取主站配置；尚未配置时返回空结构（不报错），调用方据此判断是否已就绪。
func (r *ProfitSettings) Get() (*ProfitSetting, error) {
	var s ProfitSetting
	err := r.db.Where("id = ?", profitSettingID).First(&s).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return &ProfitSetting{ID: profitSettingID}, nil
		}
		return nil, err
	}
	return &s, nil
}

// Save 覆盖写入主站配置（固定 ID=1）。
func (r *ProfitSettings) Save(s *ProfitSetting) error {
	s.ID = profitSettingID
	return r.db.Save(s).Error
}

// ProfitSnapshots 收益快照仓库。
type ProfitSnapshots struct{ db *gorm.DB }

func NewProfitSnapshots(db *gorm.DB) *ProfitSnapshots { return &ProfitSnapshots{db: db} }

// Create 追加一条快照，SampledAt 为零值时取当前时间。
func (r *ProfitSnapshots) Create(s *ProfitSnapshot) error {
	if s.SampledAt.IsZero() {
		s.SampledAt = time.Now()
	}
	return r.db.Create(s).Error
}

// LatestForDay 取指定日期（"2006-01-02"，Asia/Shanghai）最新一条快照；没有则返回 (nil, nil)。
func (r *ProfitSnapshots) LatestForDay(day string) (*ProfitSnapshot, error) {
	var s ProfitSnapshot
	err := r.db.Where("day = ?", day).Order("sampled_at DESC").Order("id DESC").First(&s).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// DeleteBefore 删除 sampled_at < cutoff 的快照，返回删除行数。
func (r *ProfitSnapshots) DeleteBefore(cutoff time.Time) (int64, error) {
	res := r.db.Where("sampled_at < ?", cutoff).Delete(&ProfitSnapshot{})
	return res.RowsAffected, res.Error
}

// DecodeProfitChannels 解析快照里的渠道明细；内容损坏时返回空切片而不是错误，
// 收益结果本身仍然可用，只是拿不到明细。
func DecodeProfitChannels(raw string) []ProfitChannelCost {
	if raw == "" {
		return nil
	}
	var list []ProfitChannelCost
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil
	}
	return list
}

// EncodeProfitChannels 序列化渠道明细为快照字段值。
func EncodeProfitChannels(list []ProfitChannelCost) string {
	if len(list) == 0 {
		return "[]"
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}
