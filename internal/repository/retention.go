package repository

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// P1 L7 保留期清理（§6.8 技术实现第 7 条，worker，SystemScope）：先删对象再置 deleted_at（行保留作墓碑）。

// RawRef 定位一行 raw_object。
type RawRef struct {
	TenantID, FarmID, ID uint64
}

// ExpiredRaw 是一行已到期、未清理的 raw_object。
type ExpiredRaw struct {
	RawRef
	Kind      model.RawKind
	ObjectKey string
	ExpireAt  time.Time
}

// RetentionCursor 是分批游标：(expire_at, id) 之后；被引用跳过的行不会让下一批原地打转。
type RetentionCursor struct {
	ExpireAt time.Time
	ID       uint64
}

// RetentionRepo 是保留期清理的仓储。
type RetentionRepo struct{ DB *gorm.DB }

// retentionEpoch 是零游标的下界（datetime 合法值，早于任何 expire_at）。
var retentionEpoch = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// ListExpired 按 §6.8 SQL 取 deleted_at IS NULL AND expire_at < now 的一批（idx_expire），在其上加 (expire_at, id) 游标。
func (r RetentionRepo) ListExpired(ctx context.Context, sys SystemScope, now time.Time, after RetentionCursor,
	limit int) ([]ExpiredRaw, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	if after.ExpireAt.IsZero() {
		after.ExpireAt = retentionEpoch
	}
	var rows []struct {
		ID, TenantID, FarmID uint64
		Kind, ObjectKey      string
		ExpireAt             time.Time
	}
	err := r.DB.WithContext(ctx).Raw(`SELECT id, tenant_id, farm_id, kind, object_key, expire_at FROM raw_object
 WHERE deleted_at IS NULL AND expire_at < ? AND (expire_at > ? OR (expire_at = ? AND id > ?)) ORDER BY expire_at, id LIMIT ?`,
		now, after.ExpireAt, after.ExpireAt, after.ID, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list expired raw_object (%s): %w", sys.Task(), err)
	}
	out := make([]ExpiredRaw, 0, len(rows))
	for _, x := range rows {
		out = append(out, ExpiredRaw{RawRef: RawRef{TenantID: x.TenantID, FarmID: x.FarmID, ID: x.ID},
			Kind: model.RawKind(x.Kind), ObjectKey: x.ObjectKey, ExpireAt: x.ExpireAt.UTC()})
	}
	return out, nil
}

// openTicketStatuses 是未关闭票据的状态。
var openTicketStatuses = []string{string(model.TicketPreparing), string(model.TicketOpen)}

// terminalOrderStatuses 返回工单终态（引用方工单处于终态时不阻止清理）。
func terminalOrderStatuses() []string {
	var out []string
	for _, s := range model.WorkOrderStatuses() {
		if s.IsTerminal() {
			out = append(out, string(s))
		}
	}
	return out
}

// terminalRunStatuses 是 agent_run 终态（run 终态后其 checkpoint 不再用于恢复，不阻止清理）。
var terminalRunStatuses = []string{string(model.RunCompleted), string(model.RunFailed), string(model.RunCanceled)}

// Referenced 返回 ids 中仍被未终态工单 / 未终态 run 或未关闭票据引用的 raw_object.id：审批快照（approval_ticket.snapshot_raw_id）、
// 下发指令（dispatch_attempt.command_raw_id）、回执原文（device_receipt.raw_object_id）、溢出的 checkpoint
// （run_checkpoint.raw_object_id，恢复中断 run 需要）。引用方的工单 / run 查不到时按被引用处理
// （失败关闭：宁可晚删）。四列均无索引，按 IN 列表扫描（每批 ≤200，量级可接受）。
func (r RetentionRepo) Referenced(ctx context.Context, sys SystemScope, ids []uint64) (map[uint64]bool, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	out := map[uint64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	term := terminalOrderStatuses()
	q := `SELECT t.snapshot_raw_id AS id FROM approval_ticket t
  LEFT JOIN work_order o ON o.tenant_id=t.tenant_id AND o.farm_id=t.farm_id AND o.id=t.order_id
 WHERE t.snapshot_raw_id IN ? AND (t.status IN ? OR o.status IS NULL OR o.status NOT IN ?)
UNION SELECT a.command_raw_id FROM dispatch_attempt a
  LEFT JOIN work_order o ON o.tenant_id=a.tenant_id AND o.farm_id=a.farm_id AND o.id=a.order_id
 WHERE a.command_raw_id IN ? AND (o.status IS NULL OR o.status NOT IN ?)
UNION SELECT d.raw_object_id FROM device_receipt d
  LEFT JOIN work_order o ON o.tenant_id=d.tenant_id AND o.farm_id=d.farm_id AND o.id=d.order_id
 WHERE d.raw_object_id IN ? AND (o.status IS NULL OR o.status NOT IN ?)
UNION SELECT c.raw_object_id FROM run_checkpoint c
  LEFT JOIN agent_run r ON r.tenant_id=c.tenant_id AND r.farm_id=c.farm_id AND r.id=c.run_id
 WHERE c.raw_object_id IN ? AND (r.status IS NULL OR r.status NOT IN ?)`
	var refs []uint64
	if err := r.DB.WithContext(ctx).Raw(q, ids, openTicketStatuses, term, ids, term, ids, term, ids, terminalRunStatuses).
		Scan(&refs).Error; err != nil {
		return nil, fmt.Errorf("repository: raw_object references (%s): %w", sys.Task(), err)
	}
	for _, id := range refs {
		out[id] = true
	}
	return out, nil
}

// MarkDeletedTx 照抄 §6.8：置 deleted_at（对象已删）；影响 0 行（已被他人清理）返回 false。
func (RetentionRepo) MarkDeletedTx(ctx context.Context, tx *gorm.DB, ref RawRef, now time.Time) (bool, error) {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return false, err
	}
	res, err := execInTx(ctx, tx, `UPDATE raw_object SET deleted_at=? WHERE tenant_id=? AND farm_id=? AND id=? AND deleted_at IS NULL`,
		now, ref.TenantID, ref.FarmID, ref.ID)
	if err != nil {
		return false, fmt.Errorf("repository: tombstone raw_object %d: %w", ref.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("repository: tombstone raw_object %d rows: %w", ref.ID, err)
	}
	return n == 1, nil
}

// BackfillExpire 按 §6.8 回填存量行 expire_at（只由 cmd/migrate up 迁移后执行的跨租户运维操作，days 键为大写 kind）。
// 返回各 kind 回填行数。
func (r RetentionRepo) BackfillExpire(ctx context.Context, days map[string]int) (map[string]int64, error) {
	kinds := make([]string, 0, len(days))
	for k := range days {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	out := make(map[string]int64, len(kinds))
	for _, k := range kinds {
		res := r.DB.WithContext(ctx).Exec(`UPDATE raw_object SET expire_at = created_at + INTERVAL ? DAY WHERE kind=? AND expire_at IS NULL`,
			days[k], strings.ToUpper(k))
		if res.Error != nil {
			return out, fmt.Errorf("repository: backfill raw_object.expire_at kind %s: %w", k, res.Error)
		}
		out[k] = res.RowsAffected
	}
	return out, nil
}
