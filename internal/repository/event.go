package repository

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"agri-steward/internal/model"
)

// EventRepo 是 run_event 的最小 EventStore 实现；audit_log 投影留给 L7。
type EventRepo struct{}

// ErrRunNotFound 表示 AppendTx 找不到对应 agent_run。
var ErrRunNotFound = errors.New("repository: agent_run not found")

// AppendTx 在调用方事务内分配 seq 并追加事件，返回分配的 seq。
// UPDATE ... LAST_INSERT_ID(event_seq + 1) 与 INSERT ... LAST_INSERT_ID() 必须同一连接，故强制事务。
func (EventRepo) AppendTx(ctx context.Context, tx *gorm.DB, e model.RunEvent) (int64, error) {
	if !e.Type.Valid() {
		return 0, fmt.Errorf("repository: unregistered event_type %q", e.Type)
	}
	res, err := execInTx(ctx, tx,
		`UPDATE agent_run SET event_seq = LAST_INSERT_ID(event_seq + 1) WHERE id = ? AND tenant_id = ? AND farm_id = ?`,
		e.RunID, e.TenantID, e.FarmID)
	if err != nil {
		return 0, fmt.Errorf("repository: alloc event seq run %d: %w", e.RunID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("repository: alloc event seq rows: %w", err)
	}
	if n == 0 {
		return 0, fmt.Errorf("%w: run %d", ErrRunNotFound, e.RunID)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("repository: read event seq: %w", err)
	}
	if _, err := execInTx(ctx, tx,
		`INSERT INTO run_event (tenant_id, farm_id, run_id, seq, step_id, tool_call_id, event_type, actor, payload, created_at) VALUES (?, ?, ?, LAST_INSERT_ID(), ?, ?, ?, ?, ?, ?)`,
		e.TenantID, e.FarmID, e.RunID, nullIfEmpty(e.StepID), e.ToolCallID, string(e.Type), string(e.Actor),
		string(e.Payload), e.CreatedAt); err != nil {
		return 0, fmt.Errorf("repository: insert run_event run %d: %w", e.RunID, err)
	}
	return seq, nil
}
