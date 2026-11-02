package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// ProfileRepo 是 farm_profile / field_profile 的 upsert（种子导入，L2 起 mcp-field 同步复用）。
type ProfileRepo struct{}

const sowingDateLayout = "2006-01-02"

// UpsertFarm 按 uk_farm upsert 农场主数据。
func (ProfileRepo) UpsertFarm(ctx context.Context, tx *gorm.DB, f model.FarmProfile, now time.Time) error {
	if err := requireFarm(f.TenantID, f.FarmID); err != nil {
		return err
	}
	crops, err := json.Marshal(f.MainCrops)
	if err != nil {
		return fmt.Errorf("repository: encode main_crops: %w", err)
	}
	err = tx.WithContext(ctx).Exec(
		`INSERT INTO farm_profile (tenant_id, farm_id, name, region, timezone, main_crops, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) AS new
  ON DUPLICATE KEY UPDATE name=new.name, region=new.region, timezone=new.timezone, main_crops=new.main_crops, row_ver=farm_profile.row_ver+1, updated_at=new.updated_at`,
		f.TenantID, f.FarmID, f.Name, f.Region, f.Timezone, string(crops), now).Error
	if err != nil {
		return fmt.Errorf("repository: upsert farm_profile (%d,%d): %w", f.TenantID, f.FarmID, err)
	}
	return nil
}

// UpsertField 按 uk_ext upsert 地块，SQL 照抄 P0 L1。
func (ProfileRepo) UpsertField(ctx context.Context, tx *gorm.DB, f model.FieldProfile, now time.Time) error {
	if err := requireFarm(f.TenantID, f.FarmID); err != nil {
		return err
	}
	prefs := f.Prefs
	if len(prefs) == 0 {
		prefs = json.RawMessage(`{}`)
	}
	err := tx.WithContext(ctx).Exec(
		`INSERT INTO field_profile (tenant_id, farm_id, ext_field_code, crop, sowing_date, area_mu, prefs, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) AS new
  ON DUPLICATE KEY UPDATE crop=new.crop, sowing_date=new.sowing_date, area_mu=new.area_mu, row_ver=field_profile.row_ver+1, updated_at=new.updated_at`,
		f.TenantID, f.FarmID, f.ExtFieldCode, f.Crop, f.SowingDate.Format(sowingDateLayout), f.AreaMu, string(prefs), now).Error
	if err != nil {
		return fmt.Errorf("repository: upsert field_profile %s: %w", f.ExtFieldCode, err)
	}
	return nil
}

// FieldIDByCode 按外部编码查 field_id（评测样本的 field-00x → bigint）。
func (ProfileRepo) FieldIDByCode(ctx context.Context, db *gorm.DB, tenantID, farmID uint64, code string) (uint64, error) {
	if err := requireFarm(tenantID, farmID); err != nil {
		return 0, err
	}
	var id uint64
	res := db.WithContext(ctx).Raw(
		`SELECT id FROM field_profile WHERE tenant_id=? AND farm_id=? AND ext_field_code=?`, tenantID, farmID, code).Scan(&id)
	if res.Error != nil {
		return 0, fmt.Errorf("repository: field %s: %w", code, res.Error)
	}
	if res.RowsAffected == 0 {
		return 0, fmt.Errorf("repository: field %s: %w", code, model.ErrNotFound)
	}
	return id, nil
}

// FieldDirectory 按 field_profile.id 查外部地块编码，满足 mcp servers 的 kit.FieldDirectory
// （mock 数据按 ext_field_code 组织）。一律按 Scope 过滤：他场地块与不存在同样返回 ErrNotFound。
type FieldDirectory struct {
	DB *gorm.DB
}

// FieldCode 返回本农场地块编码；不存在或不属于 scope 返回 model.ErrNotFound。
func (d FieldDirectory) FieldCode(ctx context.Context, scope model.Scope, fieldID uint64) (string, error) {
	if err := requireScope(scope); err != nil {
		return "", err
	}
	var code string
	res := d.DB.WithContext(ctx).Raw(`SELECT ext_field_code FROM field_profile WHERE tenant_id=? AND farm_id=? AND id=?`,
		scope.TenantID, scope.FarmID, fieldID).Scan(&code)
	if res.Error != nil {
		return "", fmt.Errorf("repository: field_profile %d: %w", fieldID, res.Error)
	}
	if res.RowsAffected == 0 {
		return "", fmt.Errorf("repository: field_profile %d: %w", fieldID, model.ErrNotFound)
	}
	return code, nil
}

// FieldIDs 返回本农场全部地块 id（升序；无地块为空切片），供评测 cross_farm_isolation 判 field_id 归属。
func (d FieldDirectory) FieldIDs(ctx context.Context, scope model.Scope) ([]uint64, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	ids := []uint64{}
	if err := d.DB.WithContext(ctx).Raw(`SELECT id FROM field_profile WHERE tenant_id=? AND farm_id=? ORDER BY id`,
		scope.TenantID, scope.FarmID).Scan(&ids).Error; err != nil {
		return nil, fmt.Errorf("repository: field_profile ids: %w", err)
	}
	return ids, nil
}
