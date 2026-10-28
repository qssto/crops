package repository

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// WorkOrderRepo 是 work_order / work_order_transition 读写；只在调用方事务内使用。
type WorkOrderRepo struct{}

// GetState 读取迁移前的状态与 row_ver（不加锁，靠 UpdateStatus 的 row_ver 条件兜底）。
func (WorkOrderRepo) GetState(ctx context.Context, tx *gorm.DB, ref model.OrderRef) (model.OrderState, error) {
	var row struct {
		RunID  uint64
		Status string
		RowVer int
	}
	res := tx.WithContext(ctx).Raw(
		`SELECT run_id, status, row_ver FROM work_order WHERE id=? AND tenant_id=? AND farm_id=?`,
		ref.OrderID, ref.TenantID, ref.FarmID).Scan(&row)
	if res.Error != nil {
		return model.OrderState{}, fmt.Errorf("repository: get work_order %d: %w", ref.OrderID, res.Error)
	}
	if res.RowsAffected == 0 {
		return model.OrderState{}, fmt.Errorf("repository: work_order %d: %w", ref.OrderID, model.ErrNotFound)
	}
	return model.OrderState{RunID: row.RunID, Status: model.WorkOrderStatus(row.Status), RowVer: row.RowVer}, nil
}

// UpdateStatus 执行 P0 L1 定版的乐观锁 UPDATE；影响行数 0 返回 model.ErrConcurrentUpdate。
func (WorkOrderRepo) UpdateStatus(ctx context.Context, tx *gorm.DB, ref model.OrderRef,
	from, to model.WorkOrderStatus, rowVer int, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE work_order SET status=?, row_ver=row_ver+1, updated_at=? WHERE id=? AND tenant_id=? AND farm_id=? AND status=? AND row_ver=?`,
		string(to), now, ref.OrderID, ref.TenantID, ref.FarmID, string(from), rowVer)
	if res.Error != nil {
		return fmt.Errorf("repository: update work_order %d status: %w", ref.OrderID, res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("repository: work_order %d row_ver %d: %w", ref.OrderID, rowVer, model.ErrConcurrentUpdate)
	}
	return nil
}

// InsertTransition 追加一条迁移记录。
func (WorkOrderRepo) InsertTransition(ctx context.Context, tx *gorm.DB, t model.OrderTransition) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	err := tx.WithContext(ctx).Exec(
		`INSERT INTO work_order_transition (tenant_id, farm_id, order_id, run_id, from_status, to_status, actor_type, actor_id, reason_code, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Ref.TenantID, t.Ref.FarmID, t.Ref.OrderID, t.RunID, string(t.From), string(t.To),
		string(t.Actor.Type), t.Actor.ID, nullIfEmpty(string(t.ReasonCode)), t.CreatedAt).Error
	if err != nil {
		return fmt.Errorf("repository: insert work_order_transition %d: %w", t.Ref.OrderID, err)
	}
	return nil
}
