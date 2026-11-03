package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// P1 L7 审计导出（§6.8 技术实现第 5 条）：不建导出表，状态与结果写在 outbox(topic=audit.export) 的 payload。

// TopicAuditExport 是审计导出的 outbox topic。
const TopicAuditExport = "audit.export"

// AuditExportRow 是一条导出请求（outbox 行）。
type AuditExportRow struct {
	ID        uint64
	Status    string // outbox.status：PENDING / DONE / FAILED / DEAD
	Attempts  int
	Payload   model.AuditExportPayload
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AuditExportRepo 读写 audit.export 的 outbox 行；WHERE 一律以 tenant_id, farm_id 开头（他场导出即 not found）。
type AuditExportRepo struct{ DB *gorm.DB }

// InsertTx 在调用方事务内写一条 audit.export（INSERT IGNORE，uk_dedup = audit.export:{farm_id}:{request_id}），
// 返回 export_id（= outbox.id）与是否新插入；同 dedup_key 已有时返回已有行的 id（同农场内幂等重放）。
func (AuditExportRepo) InsertTx(ctx context.Context, tx *gorm.DB, scope model.Scope, p model.AuditExportPayload,
	now time.Time) (uint64, bool, error) {
	if err := requireScope(scope); err != nil {
		return 0, false, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return 0, false, fmt.Errorf("repository: marshal audit export payload: %w", err)
	}
	dedup := fmt.Sprintf("%s:%d:%s", TopicAuditExport, scope.FarmID, p.RequestID)
	res, err := execInTx(ctx, tx,
		`INSERT IGNORE INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, 'PENDING', ?, ?, ?)`,
		scope.TenantID, scope.FarmID, TopicAuditExport, p.RequestedBy, dedup, string(body), now, now, now)
	if err != nil {
		return 0, false, fmt.Errorf("repository: insert outbox %s: %w", dedup, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("repository: outbox %s rows: %w", dedup, err)
	}
	if n == 1 {
		id, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("repository: outbox %s id: %w", dedup, err)
		}
		return uint64(id), true, nil
	}
	var id uint64
	r := tx.WithContext(ctx).Raw(`SELECT id FROM outbox WHERE tenant_id=? AND farm_id=? AND dedup_key=?`,
		scope.TenantID, scope.FarmID, dedup).Scan(&id)
	if r.Error != nil {
		return 0, false, fmt.Errorf("repository: outbox %s existing id: %w", dedup, r.Error)
	}
	if r.RowsAffected == 0 { // dedup_key 全局唯一，被他场占用不可能（键含 farm_id）；防御性失败关闭
		return 0, false, fmt.Errorf("repository: outbox %s: %w", dedup, model.ErrNotFound)
	}
	return id, false, nil
}

// Get 按 tenant_id, farm_id, id 查导出；查不到（含他场导出）返回 model.ErrNotFound。
func (r AuditExportRepo) Get(ctx context.Context, scope model.Scope, id uint64) (AuditExportRow, error) {
	if err := requireScope(scope); err != nil {
		return AuditExportRow{}, err
	}
	var row struct {
		ID                   uint64
		Status               string
		Attempts             int
		Payload              string
		CreatedAt, UpdatedAt time.Time
	}
	res := r.DB.WithContext(ctx).Raw(`SELECT id, status, attempts, payload, created_at, updated_at FROM outbox WHERE tenant_id=? AND farm_id=? AND id=? AND topic=?`,
		scope.TenantID, scope.FarmID, id, TopicAuditExport).Scan(&row)
	if res.Error != nil {
		return AuditExportRow{}, fmt.Errorf("repository: read audit export %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return AuditExportRow{}, fmt.Errorf("repository: audit export %d: %w", id, model.ErrNotFound)
	}
	out := AuditExportRow{ID: row.ID, Status: row.Status, Attempts: row.Attempts, CreatedAt: row.CreatedAt.UTC(),
		UpdatedAt: row.UpdatedAt.UTC()}
	if err := json.Unmarshal([]byte(row.Payload), &out.Payload); err != nil {
		return AuditExportRow{}, fmt.Errorf("repository: audit export %d payload: %w", id, err)
	}
	return out, nil
}

// SetPayloadTx 在调用方事务内回写导出 payload（状态 / raw_object_id / 失败原因）。不碰 status / row_ver：
// outbox 的完成由轮询器按租约写；行已不存在返回 model.ErrNotFound。
func (AuditExportRepo) SetPayloadTx(ctx context.Context, tx *gorm.DB, scope model.Scope, id uint64,
	p model.AuditExportPayload, now time.Time) error {
	if err := requireScope(scope); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("repository: marshal audit export payload: %w", err)
	}
	res, err := execInTx(ctx, tx, `UPDATE outbox SET payload=?, updated_at=? WHERE tenant_id=? AND farm_id=? AND id=? AND topic=?`,
		string(body), now, scope.TenantID, scope.FarmID, id, TopicAuditExport)
	if err != nil {
		return fmt.Errorf("repository: update audit export %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("repository: update audit export %d rows: %w", id, err)
	}
	if n == 0 { // 同值更新 MySQL 也报 0 行：payload 含 updated_at 之外的变化，正常不会相同；不存在才报错
		var exists int64
		if err := tx.WithContext(ctx).Raw(`SELECT COUNT(*) FROM outbox WHERE tenant_id=? AND farm_id=? AND id=? AND topic=?`,
			scope.TenantID, scope.FarmID, id, TopicAuditExport).Scan(&exists).Error; err != nil {
			return fmt.Errorf("repository: audit export %d exists: %w", id, err)
		}
		if exists == 0 {
			return fmt.Errorf("repository: audit export %d: %w", id, model.ErrNotFound)
		}
	}
	return nil
}
