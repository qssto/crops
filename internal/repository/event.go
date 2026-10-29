package repository

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"agri-steward/internal/model"
	"agri-steward/internal/repository/rediskey"
)

// EventRepo 是 run_event 的唯一写入口：同事务写 audit_log 投影（L7），提交后 PUBLISH sse:run:{run_id}。
// 零值只写库不发布（测试 / 评测）；Pub 非空时 Keys 必须有效。
type EventRepo struct {
	Pub  Publisher
	Keys rediskey.Keys
	Log  *slog.Logger
}

// ErrRunNotFound 表示 AppendTx 找不到对应 agent_run。
var ErrRunNotFound = errors.New("repository: agent_run not found")

// AppendTx 在调用方事务内分配 seq、追加事件并写 audit_log 投影，返回分配的 seq。
// UPDATE ... LAST_INSERT_ID(event_seq + 1) 与 INSERT ... LAST_INSERT_ID() 必须同一连接，故强制事务。
// SSE 发布登记为提交后动作：事务须由本包 inTx 开启（Transactor / DecisionRepo / EventSink），否则只告警不发布。
func (r EventRepo) AppendTx(ctx context.Context, tx *gorm.DB, e model.RunEvent) (int64, error) {
	if !e.Type.Valid() {
		return 0, fmt.Errorf("repository: unregistered event_type %q", e.Type)
	}
	rec, audited, err := ProjectAudit(e)
	if err != nil {
		return 0, err
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
	if audited {
		if err := (AuditRepo{}).AppendTx(ctx, tx, rec); err != nil {
			return 0, fmt.Errorf("repository: audit %s run %d: %w", e.Type, e.RunID, err)
		}
	}
	if r.Pub != nil && !onCommit(tx, func(ctx context.Context) { r.publish(ctx, e, seq) }) {
		r.warn(ctx, "sse skipped: tx not opened by repository", e, seq, nil)
	}
	return seq, nil
}
