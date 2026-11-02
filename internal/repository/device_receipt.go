package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// 回执侧新增的 outbox topic（P1 §6.5 SQL 注释）。
const (
	TopicOrderCancel        = "order.cancel"
	TopicFieldHistoryAppend = "field.history.append"
)

// ReceiptBatch 是回执超时扫描每轮最多处理的尝试数（§6.5 扫描 SQL 的 LIMIT 100）。
const ReceiptBatch = 100

// DeviceReceiptRepo 是回执侧仓储（P1 §6.5 技术实现第 5-9 条，SQL 照抄）：device_receipt 入库、回执超时扫描、
// DONE 回写与重试 / 中止 / 回写 outbox。*Tx 方法只在调用方事务内使用。
type DeviceReceiptRepo struct{ DB *gorm.DB }

// ReceiptRow 是 device_receipt 行（0009）。RawObjectID 为 0 时写 NULL。
type ReceiptRow struct {
	TenantID    uint64
	FarmID      uint64
	ReceiptID   string
	OrderID     uint64
	Attempt     int
	DeviceJobID string
	NormStatus  model.AttemptStatus
	Effect      model.ReceiptOutcome
	DoneAreaMu  *float64
	DeviceTS    time.Time
	Source      model.ReceiptSource
	RawObjectID uint64
	CreatedAt   time.Time
}

// InsertReceiptTx 照抄 §6.5 回执入库 INSERT IGNORE；返回 false 表示 uk_receipt 已有（重放）。
func (DeviceReceiptRepo) InsertReceiptTx(ctx context.Context, tx *gorm.DB, r ReceiptRow) (bool, error) {
	if err := requireFarm(r.TenantID, r.FarmID); err != nil {
		return false, err
	}
	if r.ReceiptID == "" || r.DeviceJobID == "" || !r.Source.Valid() {
		return false, fmt.Errorf("repository: insert device_receipt order %d: receipt_id / device_job_id / source required", r.OrderID)
	}
	raw := sql.NullInt64{Int64: int64(r.RawObjectID), Valid: r.RawObjectID != 0}
	res, err := execInTx(ctx, tx,
		`INSERT IGNORE INTO device_receipt (tenant_id, farm_id, receipt_id, order_id, attempt, device_job_id, norm_status, effect,
  done_area_mu, device_ts, source, raw_object_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TenantID, r.FarmID, r.ReceiptID, r.OrderID, r.Attempt, r.DeviceJobID, string(r.NormStatus), string(r.Effect),
		r.DoneAreaMu, r.DeviceTS, string(r.Source), raw, r.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("repository: insert device_receipt order %d: %w", r.OrderID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("repository: device_receipt rows: %w", err)
	}
	return n > 0, nil
}

// ReceiptsByOrder 按 device_ts 读作业单的全部回执（对账页 / 评测；文档未给 SQL，走 idx_order）。
func (r DeviceReceiptRepo) ReceiptsByOrder(ctx context.Context, ref model.OrderRef) ([]ReceiptRow, error) {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return nil, err
	}
	var rows []struct {
		TenantID    uint64
		FarmID      uint64
		ReceiptID   string
		OrderID     uint64
		Attempt     int
		DeviceJobID string
		NormStatus  string
		Effect      string
		DoneAreaMu  sql.NullFloat64
		DeviceTS    time.Time `gorm:"column:device_ts"`
		Source      string
		RawObjectID sql.NullInt64
		CreatedAt   time.Time
	}
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT tenant_id, farm_id, receipt_id, order_id, attempt, device_job_id, norm_status, effect, done_area_mu, device_ts, source,
  raw_object_id, created_at FROM device_receipt WHERE tenant_id=? AND farm_id=? AND order_id=? ORDER BY attempt, device_ts, id`,
		ref.TenantID, ref.FarmID, ref.OrderID).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: list device_receipt order %d: %w", ref.OrderID, err)
	}
	out := make([]ReceiptRow, 0, len(rows))
	for _, w := range rows {
		row := ReceiptRow{TenantID: w.TenantID, FarmID: w.FarmID, ReceiptID: w.ReceiptID, OrderID: w.OrderID, Attempt: w.Attempt,
			DeviceJobID: w.DeviceJobID, NormStatus: model.AttemptStatus(w.NormStatus), Effect: model.ReceiptOutcome(w.Effect),
			DeviceTS: w.DeviceTS, Source: model.ReceiptSource(w.Source), RawObjectID: uint64(w.RawObjectID.Int64), CreatedAt: w.CreatedAt}
		if w.DoneAreaMu.Valid {
			v := w.DoneAreaMu.Float64
			row.DoneAreaMu = &v
		}
		out = append(out, row)
	}
	return out, nil
}

// AttemptTx 在事务内按 id 读一条尝试（回执处置前的当前状态与 row_ver；文档未给 SQL）。找不到 → model.ErrNotFound。
func (DeviceReceiptRepo) AttemptTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, id uint64) (model.DispatchAttempt, error) {
	return attemptOne(ctx, tx, ref,
		`SELECT `+attemptCols+` FROM dispatch_attempt WHERE tenant_id=? AND farm_id=? AND order_id=? AND id=?`, id)
}

// LatestAttemptTx 在事务内读作业单最后一次尝试（中止 / 对账针对它；文档未给 SQL，走 uk_order_attempt）。
// 找不到 → model.ErrNotFound。
func (DeviceReceiptRepo) LatestAttemptTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef) (model.DispatchAttempt, error) {
	return attemptOne(ctx, tx, ref,
		`SELECT `+attemptCols+` FROM dispatch_attempt WHERE tenant_id=? AND farm_id=? AND order_id=? ORDER BY attempt DESC LIMIT 1`)
}

func attemptOne(ctx context.Context, db *gorm.DB, ref model.OrderRef, query string, extra ...any) (model.DispatchAttempt, error) {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return model.DispatchAttempt{}, err
	}
	if err := requireTx(db); err != nil {
		return model.DispatchAttempt{}, err
	}
	var rows []attemptRow
	args := append([]any{ref.TenantID, ref.FarmID, ref.OrderID}, extra...)
	if err := db.WithContext(ctx).Raw(query, args...).Scan(&rows).Error; err != nil {
		return model.DispatchAttempt{}, fmt.Errorf("repository: read dispatch_attempt order %d: %w", ref.OrderID, err)
	}
	if len(rows) == 0 {
		return model.DispatchAttempt{}, fmt.Errorf("repository: dispatch_attempt of order %d: %w", ref.OrderID, model.ErrNotFound)
	}
	return attempts(rows)[0], nil
}

// DueAttempt 是回执超时扫描取到的尝试（§6.5 扫描 SQL 的列）。
type DueAttempt struct {
	ID       uint64
	TenantID uint64
	FarmID   uint64
	OrderID  uint64
}

// Ref 返回尝试所属作业单。
func (d DueAttempt) Ref() model.OrderRef {
	return model.OrderRef{TenantID: d.TenantID, FarmID: d.FarmID, OrderID: d.OrderID}
}

// DueAttempts 照抄 §6.5 回执超时扫描 SQL（worker 专用索引 idx_due）；跨农场，须持 SystemScope。
func (r DeviceReceiptRepo) DueAttempts(ctx context.Context, sys SystemScope, now time.Time) ([]DueAttempt, error) {
	if err := requireSystem(sys); err != nil {
		return nil, err
	}
	var rows []DueAttempt
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT id, tenant_id, farm_id, order_id FROM dispatch_attempt
 WHERE status IN ('SENT','ACCEPTED','EXECUTING') AND receipt_due_at < ? LIMIT 100`, now).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: due dispatch_attempt (%s): %w", sys.Task(), err)
	}
	return rows, nil
}

// MarkNoReceiptTx 是超时扫描「仍无结果」：尝试置 NO_RECEIPT / failure_kind=UNKNOWN（§6.5 功能任务 3，进待对账列表），
// 只针对仍在途（SENT / ACCEPTED / EXECUTING）的尝试，row_ver 守卫；影响 0 行 → model.ErrConcurrentUpdate。
// 不改 work_order（保持 DISPATCHED）、不写 outbox（不重发）。文档未给 SQL，按 §6.3 回执 UPDATE 改写。
func (DeviceReceiptRepo) MarkNoReceiptTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, id uint64, rowVer int, now time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE dispatch_attempt SET status='NO_RECEIPT', failure_kind='UNKNOWN', row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND row_ver=? AND status IN ('SENT','ACCEPTED','EXECUTING')`,
		now, ref.TenantID, ref.FarmID, id, rowVer)
	return affected(res, fmt.Sprintf("mark dispatch_attempt %d NO_RECEIPT", id))
}

// SetDoneAreaTx 回填 work_order.done_area_mu（§6.5 第 9 步；只对已在 DONE / PARTIALLY_DONE 的单，row_ver+1）。
// 状态由 Transition 先写，本 UPDATE 同事务紧随；影响 0 行 → model.ErrConcurrentUpdate。
func (DeviceReceiptRepo) SetDoneAreaTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, doneAreaMu float64, now time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE work_order SET done_area_mu=?, row_ver=row_ver+1, updated_at=?
 WHERE tenant_id=? AND farm_id=? AND id=? AND status IN ('DONE','PARTIALLY_DONE')`,
		doneAreaMu, now, ref.TenantID, ref.FarmID, ref.OrderID)
	return affected(res, "set done_area work_order "+strconv.FormatUint(ref.OrderID, 10))
}

// InsertWorkDoneMemoryTx 照抄 §6.5 episodic_memory(WORK_DONE, ref_id=order_id)，INSERT IGNORE 幂等（uk_kind_ref）。
func (DeviceReceiptRepo) InsertWorkDoneMemoryTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, fieldID uint64,
	summary string, occurredAt time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if _, err := execInTx(ctx, tx,
		`INSERT IGNORE INTO episodic_memory (tenant_id, farm_id, field_id, kind, ref_id, summary, occurred_at) VALUES (?, ?, ?, 'WORK_DONE', ?, ?, ?)`,
		ref.TenantID, ref.FarmID, fieldID, ref.OrderID, truncateRunes(summary, 512), occurredAt); err != nil {
		return fmt.Errorf("repository: insert episodic_memory WORK_DONE order %d: %w", ref.OrderID, err)
	}
	return nil
}

// InsertFieldHistoryOutboxTx 照抄 §6.5 outbox(field.history.append:{order_id}:{ticket_id}:1)，INSERT IGNORE 幂等（uk_dedup）。
func (DeviceReceiptRepo) InsertFieldHistoryOutboxTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, ticketID uint64,
	payload []byte, now time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if _, err := execInTx(ctx, tx,
		`INSERT IGNORE INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at)
VALUES (?, ?, 'field.history.append', ?, CONCAT('field.history.append:', ?, ':', ?, ':1'), ?, 'PENDING', ?, ?, ?)`,
		ref.TenantID, ref.FarmID, ref.OrderID, ref.OrderID, ticketID, string(payload), now, now, now); err != nil {
		return fmt.Errorf("repository: insert outbox field.history.append %d: %w", ref.OrderID, err)
	}
	return nil
}

// InsertRetryDispatchOutboxTx 写下发重试消息 outbox(order.dispatch:{order_id}:{ticket_id}:{attempt})（P1-L4 接缝 2；
// 按 DecisionRepo.InsertDispatchOutboxTx 改写，INSERT IGNORE：同一失败回执重放不重复写）。attempt 须 >= 2。
func (DeviceReceiptRepo) InsertRetryDispatchOutboxTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, ticketID uint64,
	attempt int, payload []byte, now time.Time) error {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return err
	}
	if attempt < 2 {
		return fmt.Errorf("repository: retry outbox order %d: attempt %d must be >= 2", ref.OrderID, attempt)
	}
	if _, err := execInTx(ctx, tx,
		`INSERT IGNORE INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at)
VALUES (?, ?, 'order.dispatch', ?, CONCAT('order.dispatch:', ?, ':', ?, ':', ?), ?, 'PENDING', ?, ?, ?)`,
		ref.TenantID, ref.FarmID, ref.OrderID, ref.OrderID, ticketID, attempt, string(payload), now, now, now); err != nil {
		return fmt.Errorf("repository: insert outbox order.dispatch retry %d attempt %d: %w", ref.OrderID, attempt, err)
	}
	return nil
}

// InsertCancelOutboxTx 写中止消息 outbox(order.cancel:{order_id}:{ticket_id}:1)（§6.5 第 8 步），INSERT IGNORE：重复中止不重复写；
// 返回 false 表示已有同键消息。
func (DeviceReceiptRepo) InsertCancelOutboxTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, ticketID uint64,
	payload []byte, now time.Time) (bool, error) {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return false, err
	}
	res, err := execInTx(ctx, tx,
		`INSERT IGNORE INTO outbox (tenant_id, farm_id, topic, aggregate_id, dedup_key, payload, status, next_run_at, created_at, updated_at)
VALUES (?, ?, 'order.cancel', ?, CONCAT('order.cancel:', ?, ':', ?, ':1'), ?, 'PENDING', ?, ?, ?)`,
		ref.TenantID, ref.FarmID, ref.OrderID, ref.OrderID, ticketID, string(payload), now, now, now)
	if err != nil {
		return false, fmt.Errorf("repository: insert outbox order.cancel %d: %w", ref.OrderID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("repository: outbox order.cancel rows: %w", err)
	}
	return n > 0, nil
}

// WorkDoneRow 是作业单的完成态（评测 partially_done 打分器读）。
type WorkDoneRow struct {
	FieldID    uint64
	Status     model.WorkOrderStatus
	DoneAreaMu *float64
}

// WorkDone 按主键读作业单的地块、状态与 done_area_mu（文档未给 SQL）。找不到 → model.ErrNotFound。
func (r DeviceReceiptRepo) WorkDone(ctx context.Context, ref model.OrderRef) (WorkDoneRow, error) {
	if err := requireFarm(ref.TenantID, ref.FarmID); err != nil {
		return WorkDoneRow{}, err
	}
	var rows []struct {
		FieldID    uint64
		Status     string
		DoneAreaMu sql.NullFloat64
	}
	if err := r.DB.WithContext(ctx).Raw(`SELECT field_id, status, done_area_mu FROM work_order WHERE tenant_id=? AND farm_id=? AND id=?`,
		ref.TenantID, ref.FarmID, ref.OrderID).Scan(&rows).Error; err != nil {
		return WorkDoneRow{}, fmt.Errorf("repository: read work_order %d done: %w", ref.OrderID, err)
	}
	if len(rows) == 0 {
		return WorkDoneRow{}, fmt.Errorf("repository: work_order %d: %w", ref.OrderID, model.ErrNotFound)
	}
	out := WorkDoneRow{FieldID: rows[0].FieldID, Status: model.WorkOrderStatus(rows[0].Status)}
	if rows[0].DoneAreaMu.Valid {
		v := rows[0].DoneAreaMu.Float64
		out.DoneAreaMu = &v
	}
	return out, nil
}
