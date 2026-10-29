package repository

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"agri-steward/internal/agent"
	"agri-steward/internal/model"
)

// EventSink 是 EventRepo 的非事务包装（agent.EventSink）：图内事件各自开短事务追加 run_event。
// 业务事务内的事件仍直接用 EventRepo.AppendTx；audit_log 投影与 SSE 发布都在 AppendTx 内。
type EventSink struct {
	DB     *gorm.DB
	Events EventRepo
}

var _ agent.EventSink = EventSink{}

// Emit 开短事务分配 seq 并追加事件，提交后发布 SSE。
func (s EventSink) Emit(ctx context.Context, e model.RunEvent) (int64, error) {
	var seq int64
	err := inTx(ctx, s.DB, func(tx *gorm.DB) error {
		var err error
		seq, err = s.Events.AppendTx(ctx, tx, e)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("repository: emit %s run %d: %w", e.Type, e.RunID, err)
	}
	return seq, nil
}
