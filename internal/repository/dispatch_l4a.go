package repository

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// TopicNotifySend 是通知 outbox 主题（P1-L4-A：Reopen 写入「复检退回」通知；真实通道在 L5，worker 占位处理）。
const TopicNotifySend = "notify.send"

// BindDeviceTx 把选定设备写进 work_order.payload.device_id（P1 §6.5 ③，L3 遗留义务①），row_ver 守卫；
// 影响行数 0 → model.ErrConcurrentUpdate（调用方重读后重试）。device_id 为空拒绝。
func (r DispatchRepo) BindDeviceTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, deviceID string, rowVer int,
	now time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if deviceID == "" {
		return fmt.Errorf("repository: bind device work_order %d: empty device_id", ref.OrderID)
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE work_order SET payload=JSON_SET(COALESCE(payload, JSON_OBJECT()), '$.device_id', ?), row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND id=? AND row_ver=?`,
		deviceID, now, ref.TenantID, ref.FarmID, ref.OrderID, rowVer)
	return affected(res, fmt.Sprintf("bind device work_order %d", ref.OrderID))
}

// ByIdemKey 按幂等键（= outbox.dedup_key）取本农场的尝试（崩溃续跑 / 重复投递先查它，P1 §6.5 ④）；
// 找不到 → model.ErrNotFound。tx 为 nil 时用连接。
func (r DispatchAttemptRepo) ByIdemKey(ctx context.Context, tx *gorm.DB, scope model.Scope, idemKey string) (model.DispatchAttempt, error) {
	if err := requireFarm(scope.TenantID, scope.FarmID); err != nil {
		return model.DispatchAttempt{}, err
	}
	db := tx
	if db == nil {
		db = r.DB
	}
	var rows []attemptRow
	if err := db.WithContext(ctx).Raw(
		`SELECT `+attemptCols+` FROM dispatch_attempt WHERE tenant_id=? AND farm_id=? AND idem_key=?`,
		scope.TenantID, scope.FarmID, idemKey).Scan(&rows).Error; err != nil {
		return model.DispatchAttempt{}, fmt.Errorf("repository: dispatch_attempt by idem %s: %w", idemKey, err)
	}
	if len(rows) == 0 {
		return model.DispatchAttempt{}, fmt.Errorf("repository: dispatch_attempt by idem %s: %w", idemKey, model.ErrNotFound)
	}
	return attempts(rows)[0], nil
}

// OutboxEntry 是一条 outbox 消息的状态（评测观测：退回通知是否入 outbox、下发消息是否 DONE）。
type OutboxEntry struct {
	ID       uint64
	Topic    string
	DedupKey string
	Status   string
	Attempts int
}

// OutboxByAggregate 按写入顺序读本农场某聚合（作业单）的全部 outbox 消息（评测 recheck_returned 用；db 可为连接）。
func (OutboxRepo) OutboxByAggregate(ctx context.Context, db *gorm.DB, scope model.Scope, aggregateID uint64) ([]OutboxEntry, error) {
	if err := requireScope(scope); err != nil {
		return nil, err
	}
	var rows []OutboxEntry
	if err := db.WithContext(ctx).Raw(
		`SELECT id, topic, dedup_key, status, attempts FROM outbox WHERE tenant_id=? AND farm_id=? AND aggregate_id=? ORDER BY id`,
		scope.TenantID, scope.FarmID, aggregateID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list outbox aggregate %d: %w", aggregateID, err)
	}
	return rows, nil
}

// RedeliverDone 把本农场某作业单已 DONE 的指定 topic 消息重新置 PENDING、next_run_at=now（评测模拟重复投递 /
// 租约过期后另一 worker 取到同一消息；生产代码不调用），返回影响行数。
func (OutboxRepo) RedeliverDone(ctx context.Context, db *gorm.DB, scope model.Scope, aggregateID uint64, topic string,
	now time.Time) (int64, error) {
	if err := requireScope(scope); err != nil {
		return 0, err
	}
	res := db.WithContext(ctx).Exec(
		`UPDATE outbox SET status='PENDING', next_run_at=?, row_ver=row_ver+1, updated_at=? WHERE tenant_id=? AND farm_id=? AND aggregate_id=? AND topic=? AND status='DONE'`,
		now, now, scope.TenantID, scope.FarmID, aggregateID, topic)
	if res.Error != nil {
		return 0, fmt.Errorf("repository: redeliver outbox aggregate %d: %w", aggregateID, res.Error)
	}
	return res.RowsAffected, nil
}
