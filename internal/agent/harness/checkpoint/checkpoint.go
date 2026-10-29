// Package checkpoint 把 agent.CheckpointStore（repository.CheckpointRepo 实现）接成 Eino compose.CheckPointStore。
// checkpoint_id 取 run_id 十进制字符串；Eino 只在中断时 Set（L0 spike 结论 1），每次带 id 的 Invoke 开头 Get 一次。
// Set 要求 ctx 带 agent.CheckpointTarget（图在 Invoke 前放入），且其 RunID 与 checkpoint_id 一致，否则拒写：
// 实现方据此同事务落 run_checkpoint 并把 run 置 INTERRUPTED(interrupt_type)。
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/cloudwego/eino/compose"

	"agri-steward/internal/agent"
)

// ErrNoTarget 表示 Set 的 ctx 缺少 CheckpointTarget，或其 RunID 与 checkpoint_id 不符。
var ErrNoTarget = errors.New("checkpoint: missing or mismatched checkpoint target")

// ID 返回 run 的 checkpoint_id。
func ID(runID uint64) string { return strconv.FormatUint(runID, 10) }

// Store 是 compose.CheckPointStore 适配器。
type Store struct{ inner agent.CheckpointStore }

var _ compose.CheckPointStore = (*Store)(nil)

// New 包装 agent.CheckpointStore；inner 为空时返回 nil（调用方据此不挂检查点）。
func New(inner agent.CheckpointStore) *Store {
	if inner == nil {
		return nil
	}
	return &Store{inner: inner}
}

// Get 读最近一份检查点。
func (s *Store) Get(ctx context.Context, id string) ([]byte, bool, error) {
	b, ok, err := s.inner.Get(ctx, id)
	if err != nil {
		return nil, false, fmt.Errorf("checkpoint: get %s: %w", id, err)
	}
	return b, ok, nil
}

// Set 校验归属后写入。
func (s *Store) Set(ctx context.Context, id string, payload []byte) error {
	t, ok := agent.CheckpointTargetFrom(ctx)
	if !ok || ID(t.RunID) != id || t.Interrupt == "" {
		return fmt.Errorf("checkpoint: set %s: %w", id, ErrNoTarget)
	}
	if err := s.inner.Set(ctx, id, payload); err != nil {
		return fmt.Errorf("checkpoint: set %s: %w", id, err)
	}
	return nil
}
