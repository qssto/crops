package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/agent/harness/memory"
	"agri-steward/internal/model"
)

// MemoryRepo 实现 memory.Source：读 farm_profile / field_profile 与 episodic_memory（P0 §6.7 流程 5；文档未给 SQL，走 uk_farm / 主键 / idx_field_time）。
type MemoryRepo struct {
	db *gorm.DB
}

var _ memory.Source = (*MemoryRepo)(nil)

// NewMemoryRepo 构造 MemoryRepo。
func NewMemoryRepo(db *gorm.DB) *MemoryRepo { return &MemoryRepo{db: db} }

// profileRow 是画像读取列（地块与所属农场）。
type profileRow struct {
	FarmName  string
	Region    string
	MainCrops string
	FieldCode string
	Crop      string
	Sowing    time.Time
	AreaMu    string
	Prefs     string
}

// Profile 实现 memory.Source：地块不在 Scope 内 Found=false。
func (r *MemoryRepo) Profile(ctx context.Context, scope model.Scope, fieldID uint64) (memory.Profile, error) {
	var row profileRow
	res := r.db.WithContext(ctx).Raw(
		`SELECT p.name AS farm_name, p.region, p.main_crops, f.ext_field_code AS field_code, f.crop, f.sowing_date AS sowing, f.area_mu, f.prefs
 FROM field_profile f JOIN farm_profile p ON p.tenant_id=f.tenant_id AND p.farm_id=f.farm_id
 WHERE f.id=? AND f.tenant_id=? AND f.farm_id=?`,
		fieldID, scope.TenantID, scope.FarmID).Scan(&row)
	if res.Error != nil {
		return memory.Profile{}, fmt.Errorf("repository: get profile field %d: %w", fieldID, res.Error)
	}
	if res.RowsAffected == 0 {
		return memory.Profile{}, nil
	}
	summary, err := row.summary()
	if err != nil {
		return memory.Profile{}, fmt.Errorf("repository: profile field %d: %w", fieldID, err)
	}
	return memory.Profile{Summary: summary, Found: true}, nil
}

// summary 渲染画像摘要：农场 / 区域 / 常种作物 / 地块作物与播期 / 偏好（prefs 原样按键值列出）。
func (p profileRow) summary() (string, error) {
	var crops []string
	if err := json.Unmarshal([]byte(p.MainCrops), &crops); err != nil {
		return "", fmt.Errorf("main_crops: %w", err)
	}
	var prefs map[string]any
	if err := json.Unmarshal([]byte(p.Prefs), &prefs); err != nil {
		return "", fmt.Errorf("prefs: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "农场%s（%s），常种%s；地块%s种%s，播种于%s，面积%s亩",
		p.FarmName, p.Region, strings.Join(crops, "、"), p.FieldCode, p.Crop, p.Sowing.Format(time.DateOnly), p.AreaMu)
	if len(prefs) > 0 {
		raw, err := json.Marshal(prefs) // 键有序，摘要稳定
		if err != nil {
			return "", fmt.Errorf("prefs: %w", err)
		}
		fmt.Fprintf(&b, "；偏好%s", raw)
	}
	return b.String(), nil
}

// Episodic 实现 memory.Source：该地块近 limit 条 episodic_memory，按 occurred_at 倒序（idx_field_time）。
func (r *MemoryRepo) Episodic(ctx context.Context, scope model.Scope, fieldID uint64, limit int) ([]agent.MemoryItem, error) {
	var rows []agent.MemoryItem
	if err := r.db.WithContext(ctx).Raw(
		`SELECT kind, ref_id, summary, occurred_at FROM episodic_memory
 WHERE tenant_id=? AND farm_id=? AND field_id=? ORDER BY occurred_at DESC LIMIT ?`,
		scope.TenantID, scope.FarmID, fieldID, limit).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list episodic_memory field %d: %w", fieldID, err)
	}
	return rows, nil
}
