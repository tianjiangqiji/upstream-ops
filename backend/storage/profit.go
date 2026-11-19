package storage

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

// ProfitSettings 收益统计的全局设置，表里只有一行（ID=1）。
//
// 支持多个自有站点后，这张表只保留 Enabled（收益统计总开关）；
// SiteURL / 凭据等字段为兼容旧版本保留，AutoMigrate 会把旧配置迁移成
// profit_sites 里的一条记录（见 migrateLegacyProfitSite）。
type ProfitSettings struct{ db *gorm.DB }

func NewProfitSettings(db *gorm.DB) *ProfitSettings { return &ProfitSettings{db: db} }

// profitSettingID 单行配置的固定主键。
const profitSettingID uint = 1

// Get 读取全局设置；尚未配置时返回空结构（不报错），调用方据此判断是否已就绪。
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

// Save 覆盖写入全局设置（固定 ID=1）。
func (r *ProfitSettings) Save(s *ProfitSetting) error {
	s.ID = profitSettingID
	return r.db.Save(s).Error
}

// ProfitSites 自有站点仓库：收益统计的每个站点独立拉取总额与管理员消费。
type ProfitSites struct{ db *gorm.DB }

func NewProfitSites(db *gorm.DB) *ProfitSites { return &ProfitSites{db: db} }

// List 按展示顺序返回全部站点。
func (r *ProfitSites) List() ([]ProfitSite, error) {
	var list []ProfitSite
	if err := r.db.Order("sort_order DESC").Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// ListEnabled 只返回启用的站点，收益更新用它决定本轮要打哪些站。
func (r *ProfitSites) ListEnabled() ([]ProfitSite, error) {
	var list []ProfitSite
	if err := r.db.Where("enabled = ?", true).Order("sort_order DESC").Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

func (r *ProfitSites) FindByID(id uint) (*ProfitSite, error) {
	var s ProfitSite
	if err := r.db.First(&s, id).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *ProfitSites) Create(s *ProfitSite) error { return r.db.Create(s).Error }

func (r *ProfitSites) Update(s *ProfitSite) error { return r.db.Save(s).Error }

func (r *ProfitSites) Delete(id uint) error { return r.db.Delete(&ProfitSite{}, id).Error }

// Count 站点总数，用于判断是否需要从旧版单站配置迁移。
func (r *ProfitSites) Count() (int64, error) {
	var n int64
	if err := r.db.Model(&ProfitSite{}).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
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

// DecodeProfitSites 解析快照里的站点明细；损坏时返回空切片。
func DecodeProfitSites(raw string) []ProfitSiteProfit {
	if raw == "" {
		return nil
	}
	var list []ProfitSiteProfit
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil
	}
	return list
}

// EncodeProfitSites 序列化站点明细为快照字段值。
func EncodeProfitSites(list []ProfitSiteProfit) string {
	if len(list) == 0 {
		return "[]"
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}
