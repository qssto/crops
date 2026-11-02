package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// ErrFlagWriteRace 表示同一行并发首次写入在 InnoDB 间隙锁上死锁（1213），调用方按冲突处理、可重试。
var ErrFlagWriteRace = errors.New("repository: concurrent feature_flag write")

// ErrBadFlagRow 表示 feature_flag 行的 scope_type 与 (tenant_id, farm_id) 不符。
var ErrBadFlagRow = errors.New("repository: feature_flag scope_type mismatch")

// mysqlDeadlock 是 ER_LOCK_DEADLOCK。
const mysqlDeadlock = 1213

// flagWriteErr 把死锁映射为 ErrFlagWriteRace，其余原样包装。
func flagWriteErr(op string, w model.FlagWrite, err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == mysqlDeadlock {
		err = fmt.Errorf("%w: %w", ErrFlagWriteRace, err)
	}
	return fmt.Errorf("repository: %s feature_flag %s (%d,%d): %w", op, w.Key, w.TenantID, w.FarmID, err)
}

// FlagRepo 读写 feature_flag（平台级表，按 (tenant_id, farm_id, flag_key) 定位；SQL 照抄 P1 §6.1 / §6.9）。
// 读取都直读 MySQL；进程内缓存由 service/flag 负责。
type FlagRepo struct{ DB *gorm.DB }

// Rows 一次取 GLOBAL、TENANT、FARM 三层（P1 §6.1 读取 SQL），由调用方合并。
func (r FlagRepo) Rows(ctx context.Context, key model.FlagKey, tenantID, farmID uint64) ([]model.FlagRow, error) {
	var rows []struct {
		TenantID  uint64
		FarmID    uint64
		ScopeType string
		Value     []byte
	}
	err := r.DB.WithContext(ctx).Raw(
		`SELECT tenant_id, farm_id, scope_type, value FROM feature_flag
 WHERE flag_key=? AND ((tenant_id=0 AND farm_id=0) OR (tenant_id=? AND farm_id IN (0, ?)))`,
		string(key), tenantID, farmID).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repository: feature_flag %s (%d,%d): %w", key, tenantID, farmID, err)
	}
	out := make([]model.FlagRow, 0, len(rows))
	for _, row := range rows {
		// scope_type 列必须与 (tenant_id, farm_id) 一致；手工改坏的行报错，调用方按关处理（fail-closed）。
		st, err := model.FlagScopeOf(row.TenantID, row.FarmID)
		if err != nil || string(st) != row.ScopeType {
			return nil, fmt.Errorf("repository: feature_flag %s (%d,%d) scope_type %q mismatch: %w", key,
				row.TenantID, row.FarmID, row.ScopeType, errors.Join(ErrBadFlagRow, err))
		}
		out = append(out, model.FlagRow{ScopeType: st, Value: rawJSON(row.Value)})
	}
	return out, nil
}

// FlagCurrent 是一行开关写入前的旧值（审计用）。
type FlagCurrent struct {
	Value  []byte
	RowVer int
}

// LockTx 在调用方事务内锁定一行并返回旧值；行不存在返回 found=false（文档未给 SQL，供审计取旧值）。
func (FlagRepo) LockTx(ctx context.Context, tx *gorm.DB, key model.FlagKey, tenantID, farmID uint64) (FlagCurrent, bool, error) {
	if err := requireTx(tx); err != nil {
		return FlagCurrent{}, false, err
	}
	var cur FlagCurrent
	res := tx.WithContext(ctx).Raw(
		`SELECT value, row_ver FROM feature_flag WHERE tenant_id=? AND farm_id=? AND flag_key=? FOR UPDATE`,
		tenantID, farmID, string(key)).Scan(&cur)
	if res.Error != nil {
		return FlagCurrent{}, false, fmt.Errorf("repository: lock feature_flag %s (%d,%d): %w", key, tenantID, farmID, res.Error)
	}
	return cur, res.RowsAffected > 0, nil
}

// UpsertTx 是写入①：首次设置与 kill switch 用 upsert，行不存在也能一次写入。
func (FlagRepo) UpsertTx(ctx context.Context, tx *gorm.DB, w model.FlagWrite) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	err := tx.WithContext(ctx).Exec(
		`INSERT INTO feature_flag (tenant_id, farm_id, flag_key, scope_type, value, comment, updated_by, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
ON DUPLICATE KEY UPDATE value=new.value, comment=new.comment, updated_by=new.updated_by,
  row_ver=feature_flag.row_ver+1, updated_at=new.updated_at`,
		w.TenantID, w.FarmID, string(w.Key), string(w.ScopeType), string(w.Value), w.Comment, w.UpdatedBy, w.At, w.At).Error
	if err != nil {
		return flagWriteErr("upsert", w, err)
	}
	return nil
}

// InsertMissingTx 供种子导入：行不存在才插入，已有行（管理员改过的值）原样保留（updated_by=0）。
func (FlagRepo) InsertMissingTx(ctx context.Context, tx *gorm.DB, w model.FlagWrite) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	err := tx.WithContext(ctx).Exec(
		`INSERT INTO feature_flag (tenant_id, farm_id, flag_key, scope_type, value, comment, updated_by, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)
ON DUPLICATE KEY UPDATE row_ver=row_ver`,
		w.TenantID, w.FarmID, string(w.Key), string(w.ScopeType), string(w.Value), w.Comment, w.At, w.At).Error
	if err != nil {
		return fmt.Errorf("repository: seed feature_flag %s (%d,%d): %w", w.Key, w.TenantID, w.FarmID, err)
	}
	return nil
}

// UpdateTx 是写入②：修改已有行走乐观锁；影响 0 行（行不存在或 row_ver 已变）返回 false。
func (FlagRepo) UpdateTx(ctx context.Context, tx *gorm.DB, w model.FlagWrite, expectRowVer int) (bool, error) {
	if err := requireTx(tx); err != nil {
		return false, err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE feature_flag SET value=?, comment=?, updated_by=?, row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND flag_key=? AND row_ver=?`,
		string(w.Value), w.Comment, w.UpdatedBy, w.At, w.TenantID, w.FarmID, string(w.Key), expectRowVer)
	if res.Error != nil {
		return false, fmt.Errorf("repository: update feature_flag %s (%d,%d): %w", w.Key, w.TenantID, w.FarmID, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// List 是灰度档位一览（P1 §6.9 末尾 SQL，flag_key 参数化，另带 scope_type / row_ver 供乐观锁）。
func (r FlagRepo) List(ctx context.Context, key model.FlagKey) ([]model.FlagSetting, error) {
	var rows []struct {
		TenantID  uint64
		FarmID    uint64
		ScopeType string
		Value     []byte
		Comment   string
		RowVer    int
		UpdatedAt time.Time
	}
	err := r.DB.WithContext(ctx).Raw(
		`SELECT tenant_id, farm_id, scope_type, value, comment, row_ver, updated_at FROM feature_flag
 WHERE flag_key=? AND scope_type IN ('GLOBAL','TENANT','FARM') ORDER BY tenant_id, farm_id`, string(key)).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list feature_flag %s: %w", key, err)
	}
	out := make([]model.FlagSetting, 0, len(rows))
	for _, row := range rows {
		out = append(out, model.FlagSetting{TenantID: row.TenantID, FarmID: row.FarmID,
			ScopeType: model.FlagScopeType(row.ScopeType), Value: rawJSON(row.Value), Comment: row.Comment,
			RowVer: row.RowVer, UpdatedAt: row.UpdatedAt})
	}
	return out, nil
}

// ScopeExists 校验开关作用域指向已登记的农场：FARM 行查该农场，TENANT 行查租户下任一农场（文档未给 SQL）。
func (r FlagRepo) ScopeExists(ctx context.Context, tenantID, farmID uint64) (bool, error) {
	var n int
	res := r.DB.WithContext(ctx).Raw(
		`SELECT 1 FROM farm_profile WHERE tenant_id=? AND (farm_id=? OR ?=0) LIMIT 1`, tenantID, farmID, farmID).Scan(&n)
	if res.Error != nil {
		return false, fmt.Errorf("repository: farm_profile (%d,%d): %w", tenantID, farmID, res.Error)
	}
	return res.RowsAffected > 0, nil
}
