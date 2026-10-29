package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// outbox 状态与 topic（P0 §6.9 ② DDL 注释）。
const (
	OutboxPending = "PENDING"
	OutboxDone    = "DONE"
	OutboxDead    = "DEAD"
	// TopicOrderDispatch 是审批通过后写入的下发消息（DecisionRepo.InsertDispatchOutboxTx）。
	TopicOrderDispatch = "order.dispatch"
	// OutboxMaxAttempts 是转 DEAD 前的失败次数上限（与退避 SQL 中的 5 一致）。
	OutboxMaxAttempts = 5
	// outboxLastErrorMax 是 last_error 列宽（varchar(512)）。
	outboxLastErrorMax = 512
)

// OutboxMessage 是一条已领取（租约内）的 outbox 消息。
type OutboxMessage struct {
	ID          uint64
	TenantID    uint64
	FarmID      uint64
	Topic       string
	AggregateID uint64
	Payload     json.RawMessage `gorm:"-"`
	// Attempts 是本次处理前已失败的次数。
	Attempts int
}

// OutboxRepo 是 outbox 轮询 / 完成 / 退避（P0 §6.9 ② 定版 SQL 照抄）；跨租户扫描，只由 server 内轮询器使用。
type OutboxRepo struct{ DB *gorm.DB }

// OutboxBatch 是每轮最多领取的消息数（② 轮询 SQL 的 LIMIT 50）。
const OutboxBatch = 50

// Claim 在一个事务内取到期的 PENDING 消息（FOR UPDATE SKIP LOCKED，多实例互斥）并把 next_run_at 推到 leaseUntil 作租约，
// 返回领取的消息（按 next_run_at 顺序）。读消息正文的 SELECT 文档未给，按主键回读。
func (r OutboxRepo) Claim(ctx context.Context, now, leaseUntil time.Time) ([]OutboxMessage, error) {
	var out []OutboxMessage
	err := inTx(ctx, r.DB, func(tx *gorm.DB) error {
		var ids []uint64
		if err := tx.Raw(
			`SELECT id FROM outbox WHERE status='PENDING' AND next_run_at<=? ORDER BY next_run_at LIMIT 50 FOR UPDATE SKIP LOCKED`,
			now).Scan(&ids).Error; err != nil {
			return fmt.Errorf("repository: poll outbox: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.Exec(`UPDATE outbox SET next_run_at=?, row_ver=row_ver+1, updated_at=? WHERE id IN (?)`,
			leaseUntil, now, ids).Error; err != nil {
			return fmt.Errorf("repository: lease outbox: %w", err)
		}
		var rows []struct {
			OutboxMessage
			PayloadRaw []byte `gorm:"column:payload"`
		}
		if err := tx.Raw(
			`SELECT id, tenant_id, farm_id, topic, aggregate_id, payload, attempts FROM outbox WHERE id IN (?) ORDER BY next_run_at, id`,
			ids).Scan(&rows).Error; err != nil {
			return fmt.Errorf("repository: read outbox: %w", err)
		}
		out = make([]OutboxMessage, 0, len(rows))
		for _, row := range rows {
			m := row.OutboxMessage
			m.Payload = rawJSON(row.PayloadRaw)
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkDone 照抄 ② 完成 UPDATE；已 DONE（如 Dispatch 在 ⑦ 同事务置过）时影响 0 行，返回 false。
func (r OutboxRepo) MarkDone(ctx context.Context, id uint64, now time.Time) (bool, error) {
	return markDone(r.DB.WithContext(ctx), id, now)
}

// MarkDoneTx 同 MarkDone，但在调用方事务内执行（recordResult / 过期收尾同事务置 DONE）。
func (OutboxRepo) MarkDoneTx(ctx context.Context, tx *gorm.DB, id uint64, now time.Time) (bool, error) {
	if err := requireTx(tx); err != nil {
		return false, err
	}
	return markDone(tx.WithContext(ctx), id, now)
}

func markDone(db *gorm.DB, id uint64, now time.Time) (bool, error) {
	res := db.Exec(`UPDATE outbox SET status='DONE', row_ver=row_ver+1, updated_at=? WHERE id=? AND status='PENDING'`, now, id)
	if res.Error != nil {
		return false, fmt.Errorf("repository: outbox %d done: %w", id, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// MarkRetry 照抄 ② 失败退避 UPDATE（status 写在 attempts 前：MySQL 按书写顺序赋值，IF 读到的是旧 attempts）；
// 第 OutboxMaxAttempts 次失败转 DEAD。影响 0 行（已 DONE / 已 DEAD）返回 false。
func (r OutboxRepo) MarkRetry(ctx context.Context, id uint64, nextRunAt time.Time, lastErr string, now time.Time) (bool, error) {
	res := r.DB.WithContext(ctx).Exec(
		`UPDATE outbox SET status=IF(attempts+1>=5,'DEAD','PENDING'), attempts=attempts+1, next_run_at=?, last_error=?,
       row_ver=row_ver+1, updated_at=? WHERE id=? AND status='PENDING'`,
		nextRunAt, truncateRunes(lastErr, outboxLastErrorMax), now, id)
	if res.Error != nil {
		return false, fmt.Errorf("repository: outbox %d retry: %w", id, res.Error)
	}
	return res.RowsAffected > 0, nil
}

// SetDispatchResultTx 照抄 ② recordResult 的 work_order UPDATE（DISPATCHED、无结果、row_ver 守卫）；
// 影响 0 行返回 model.ErrConcurrentUpdate。
func (OutboxRepo) SetDispatchResultTx(ctx context.Context, tx *gorm.DB, ref model.OrderRef, result []byte,
	rowVer int, now time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	res := tx.WithContext(ctx).Exec(
		`UPDATE work_order SET dispatch_result=?, row_ver=row_ver+1, updated_at=?
 WHERE id=? AND tenant_id=? AND farm_id=? AND status='DISPATCHED' AND dispatch_result IS NULL AND row_ver=?`,
		string(result), now, ref.OrderID, ref.TenantID, ref.FarmID, rowVer)
	return affected(res, "dispatch result work_order "+strconv.FormatUint(ref.OrderID, 10))
}

// EventBackfillRepo 是 SSE 补读（与 outbox 同属 ② SQL，放在本文件）。
type EventBackfillRepo struct{ DB *gorm.DB }

// EventsAfter 是 ② 的 SSE 补读（seq > after，按 seq 升序）；SELECT 列在文档 seq / event_type / payload 之外
// 补 step_id / tool_call_id / actor / created_at（SSE data 契约需要），WHERE / ORDER 照抄。
func (r EventBackfillRepo) EventsAfter(ctx context.Context, scope model.Scope, runID uint64, after int64) ([]SSEMessage, error) {
	var rows []struct {
		Seq        int64
		EventType  string
		StepID     *string
		ToolCallID *uint64
		Actor      string
		Payload    []byte
		CreatedAt  time.Time
	}
	if err := r.DB.WithContext(ctx).Raw(
		`SELECT seq, event_type, step_id, tool_call_id, actor, payload, created_at FROM run_event
 WHERE tenant_id=? AND farm_id=? AND run_id=? AND seq > ? ORDER BY seq`,
		scope.TenantID, scope.FarmID, runID, after).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("repository: run_event after seq %d run %d: %w", after, runID, err)
	}
	out := make([]SSEMessage, 0, len(rows))
	for _, row := range rows {
		m := SSEMessage{RunID: runID, Seq: row.Seq, EventType: model.EventType(row.EventType), ToolCallID: row.ToolCallID,
			Actor: model.ActorType(row.Actor), Payload: rawJSON(row.Payload), CreatedAt: row.CreatedAt}
		if row.StepID != nil {
			m.StepID = *row.StepID
		}
		out = append(out, m)
	}
	return out, nil
}
